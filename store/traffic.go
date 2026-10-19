package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type TrafficRow struct {
	TS       int64
	Label    string
	Process  string
	Class    string
	Dir      string
	BytesIn  int64
	BytesOut int64
}

type EndpointRow struct {
	TS       int64 // hour
	Label    string
	Remote   string
	Port     int
	Class    string
	Dir      string
	BytesIn  int64
	BytesOut int64
}

type DestinationRow struct {
	TS       int64 // hour
	Remote   string
	BytesIn  int64
	BytesOut int64
}

type SystemRow struct {
	TS       int64
	Kind     string
	BytesIn  int64
	BytesOut int64
}

// Write stores one flush. Every table is upsert-add, so a key written twice
// (e.g. an endpoint hour spanning many flushes) accumulates.
func (db *DB) Write(traffic []TrafficRow, endpoints []EndpointRow, system []SystemRow, dests []DestinationRow) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if len(traffic) > 0 {
		st, err := tx.Prepare(`INSERT INTO traffic (ts,label,process,class,dir,bytes_in,bytes_out) VALUES (?,?,?,?,?,?,?)
			ON CONFLICT DO UPDATE SET bytes_in=bytes_in+excluded.bytes_in, bytes_out=bytes_out+excluded.bytes_out`)
		if err != nil {
			return err
		}
		for _, r := range traffic {
			if _, err := st.Exec(r.TS, r.Label, r.Process, r.Class, r.Dir, r.BytesIn, r.BytesOut); err != nil {
				return err
			}
		}
		st.Close()
	}
	if len(endpoints) > 0 {
		st, err := tx.Prepare(`INSERT INTO endpoints (ts,label,remote,port,class,dir,bytes_in,bytes_out) VALUES (?,?,?,?,?,?,?,?)
			ON CONFLICT DO UPDATE SET bytes_in=bytes_in+excluded.bytes_in, bytes_out=bytes_out+excluded.bytes_out`)
		if err != nil {
			return err
		}
		for _, r := range endpoints {
			if _, err := st.Exec(r.TS, r.Label, r.Remote, r.Port, r.Class, r.Dir, r.BytesIn, r.BytesOut); err != nil {
				return err
			}
		}
		st.Close()
	}
	if len(system) > 0 {
		st, err := tx.Prepare(`INSERT INTO system (ts,kind,bytes_in,bytes_out) VALUES (?,?,?,?)
			ON CONFLICT DO UPDATE SET bytes_in=bytes_in+excluded.bytes_in, bytes_out=bytes_out+excluded.bytes_out`)
		if err != nil {
			return err
		}
		for _, r := range system {
			if _, err := st.Exec(r.TS, r.Kind, r.BytesIn, r.BytesOut); err != nil {
				return err
			}
		}
		st.Close()
	}
	if len(dests) > 0 {
		st, err := tx.Prepare(`INSERT INTO destinations (ts,remote,bytes_in,bytes_out) VALUES (?,?,?,?)
			ON CONFLICT DO UPDATE SET bytes_in=bytes_in+excluded.bytes_in, bytes_out=bytes_out+excluded.bytes_out`)
		if err != nil {
			return err
		}
		for _, r := range dests {
			if _, err := st.Exec(r.TS, r.Remote, r.BytesIn, r.BytesOut); err != nil {
				return err
			}
		}
		st.Close()
	}
	return tx.Commit()
}

// Rollup moves minute rows older than minuteDays into the hourly tables and
// deletes endpoint rows older than endpointDays. Cutoffs are hour-aligned so
// an hour is always rolled up whole.
func (db *DB) Rollup(minuteDays, endpointDays int) error {
	now := time.Now().Unix()
	cutoff := (now - int64(minuteDays)*86400) / 3600 * 3600
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmts := []string{
		`INSERT INTO traffic_hourly (ts,label,process,class,dir,bytes_in,bytes_out)
		 SELECT ts/3600*3600, label, process, class, dir, SUM(bytes_in), SUM(bytes_out)
		 FROM traffic WHERE ts < ?1 GROUP BY 1,2,3,4,5
		 ON CONFLICT DO UPDATE SET bytes_in=bytes_in+excluded.bytes_in, bytes_out=bytes_out+excluded.bytes_out`,
		`DELETE FROM traffic WHERE ts < ?1`,
		`INSERT INTO system_hourly (ts,kind,bytes_in,bytes_out)
		 SELECT ts/3600*3600, kind, SUM(bytes_in), SUM(bytes_out)
		 FROM system WHERE ts < ?1 GROUP BY 1,2
		 ON CONFLICT DO UPDATE SET bytes_in=bytes_in+excluded.bytes_in, bytes_out=bytes_out+excluded.bytes_out`,
		`DELETE FROM system WHERE ts < ?1`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s, cutoff); err != nil {
			return fmt.Errorf("rollup: %w", err)
		}
	}
	for _, t := range []string{"endpoints", "destinations"} {
		if _, err := tx.Exec(`DELETE FROM `+t+` WHERE ts < ?`, now-int64(endpointDays)*86400); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---- queries ----------------------------------------------------------------

// Both minute and hourly tables cover disjoint time ranges, so a period
// query reads their union.
const trafficUnion = `(SELECT ts,label,process,class,dir,bytes_in,bytes_out FROM traffic WHERE ts >= ?1 AND ts < ?2
	UNION ALL SELECT ts,label,process,class,dir,bytes_in,bytes_out FROM traffic_hourly WHERE ts >= ?1 AND ts < ?2)`
const systemUnion = `(SELECT ts,kind,bytes_in,bytes_out FROM system WHERE ts >= ?1 AND ts < ?2
	UNION ALL SELECT ts,kind,bytes_in,bytes_out FROM system_hourly WHERE ts >= ?1 AND ts < ?2)`

type InOut struct {
	In  int64 `json:"in"`
	Out int64 `json:"out"`
}

// SystemTotals returns ground-truth totals per kind ("internet", "if:en0", ...).
func (db *DB) SystemTotals(from, to int64) (map[string]InOut, error) {
	rows, err := db.Query(`SELECT kind, SUM(bytes_in), SUM(bytes_out) FROM `+systemUnion+` GROUP BY kind`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	res := map[string]InOut{}
	for rows.Next() {
		var k string
		var v InOut
		if err := rows.Scan(&k, &v.In, &v.Out); err != nil {
			return nil, err
		}
		res[k] = v
	}
	return res, rows.Err()
}

// ClassTotals returns attributed totals per class, optionally for one label.
func (db *DB) ClassTotals(from, to int64, label string) (map[string]InOut, error) {
	q := `SELECT class, SUM(bytes_in), SUM(bytes_out) FROM ` + trafficUnion
	args := []any{from, to}
	if label != "" {
		q += ` WHERE label = ?3`
		args = append(args, label)
	}
	rows, err := db.Query(q+` GROUP BY class`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	res := map[string]InOut{}
	for rows.Next() {
		var k string
		var v InOut
		if err := rows.Scan(&k, &v.In, &v.Out); err != nil {
			return nil, err
		}
		res[k] = v
	}
	return res, rows.Err()
}

// WhoRow is one label (or process) with its traffic split by class and
// direction: key "internet", "internet/served", "internet/fetched", ...
type WhoRow struct {
	Name    string           `json:"name"`
	Label   string           `json:"label,omitempty"`
	Classes map[string]InOut `json:"classes"`
}

// Who groups traffic by label, or by process within one label.
func (db *DB) Who(from, to int64, label string) ([]WhoRow, error) {
	group := "label"
	q := `SELECT label, %s, class, dir, SUM(bytes_in), SUM(bytes_out) FROM ` + trafficUnion
	args := []any{from, to}
	if label != "" {
		group = "process"
		q += ` WHERE label = ?3`
		args = append(args, label)
	}
	q = fmt.Sprintf(q, group) + ` GROUP BY 1,2,3,4`
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byName := map[string]*WhoRow{}
	var order []string
	for rows.Next() {
		var lbl, name, class, dir string
		var in, out int64
		if err := rows.Scan(&lbl, &name, &class, &dir, &in, &out); err != nil {
			return nil, err
		}
		r := byName[name]
		if r == nil {
			r = &WhoRow{Name: name, Classes: map[string]InOut{}}
			if label != "" {
				r.Label = lbl
			}
			byName[name] = r
			order = append(order, name)
		}
		add(r.Classes, class, in, out)
		if dir != "" {
			add(r.Classes, class+"/"+dir, in, out)
		}
	}
	res := make([]WhoRow, 0, len(order))
	for _, n := range order {
		res = append(res, *byName[n])
	}
	return res, rows.Err()
}

func add(m map[string]InOut, k string, in, out int64) {
	v := m[k]
	v.In += in
	v.Out += out
	m[k] = v
}

type WhereRow struct {
	Remote  string `json:"remote"`
	Port    int    `json:"port"`
	Dir     string `json:"dir"`
	Class   string `json:"class"`
	Labels  string `json:"labels"`
	In      int64  `json:"in"`
	Out     int64  `json:"out"`
	PTR     string `json:"ptr"`
	ASN     string `json:"asn"`
	Org     string `json:"org"`
	Country string `json:"country"`
}

// Where lists remote endpoints, biggest first by the chosen sort ("out",
// "in" or "total"). Endpoint rows are hourly, so from is rounded down.
func (db *DB) Where(from, to int64, label, class, sort string, limit int) ([]WhereRow, error) {
	var cond []string
	args := []any{from / 3600 * 3600, to}
	if label != "" {
		args = append(args, label)
		cond = append(cond, fmt.Sprintf("e.label = ?%d", len(args)))
	}
	if class != "" {
		args = append(args, class)
		cond = append(cond, fmt.Sprintf("e.class = ?%d", len(args)))
	}
	where := ""
	if len(cond) > 0 {
		where = " AND " + strings.Join(cond, " AND ")
	}
	order := "SUM(e.bytes_out)"
	switch sort {
	case "in":
		order = "SUM(e.bytes_in)"
	case "total":
		order = "SUM(e.bytes_in)+SUM(e.bytes_out)"
	}
	args = append(args, limit)
	q := fmt.Sprintf(`SELECT e.remote, e.port, e.dir, e.class, GROUP_CONCAT(DISTINCT e.label),
			SUM(e.bytes_in), SUM(e.bytes_out),
			COALESCE(i.ptr,''), COALESCE(i.asn,''), COALESCE(i.org,''), COALESCE(i.country,'')
		FROM endpoints e LEFT JOIN ip_info i ON i.ip = e.remote
		WHERE e.ts >= ?1 AND e.ts < ?2%s
		GROUP BY e.remote, e.port, e.dir, e.class
		ORDER BY %s DESC LIMIT ?%d`, where, order, len(args))
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var res []WhereRow
	for rows.Next() {
		var r WhereRow
		if err := rows.Scan(&r.Remote, &r.Port, &r.Dir, &r.Class, &r.Labels, &r.In, &r.Out, &r.PTR, &r.ASN, &r.Org, &r.Country); err != nil {
			return nil, err
		}
		res = append(res, r)
	}
	return res, rows.Err()
}

// DestRow is one internet address: measured bytes (all processes, from its
// host route) next to what per-process connections account for.
type DestRow struct {
	Remote  string `json:"remote"`
	In      int64  `json:"in"`
	Out     int64  `json:"out"`
	AttrIn  int64  `json:"attr_in"`
	AttrOut int64  `json:"attr_out"`
	Labels  string `json:"labels"`
	Ports   string `json:"ports"`
	Served  bool   `json:"served"`
	PTR     string `json:"ptr"`
	ASN     string `json:"asn"`
	Org     string `json:"org"`
	Country string `json:"country"`
}

// Destinations lists internet addresses by measured bytes. With a label, only
// addresses that label talked to, and its share of them.
func (db *DB) Destinations(from, to int64, label, sort string, limit int) ([]DestRow, error) {
	order := "d_out"
	switch sort {
	case "in":
		order = "d_in"
	case "total":
		order = "d_in + d_out"
	}
	labelCond := ""
	args := []any{from / 3600 * 3600, to, limit}
	if label != "" {
		labelCond = " AND label = ?4"
		args = append(args, label)
	}
	q := fmt.Sprintf(`
		WITH d AS (SELECT remote, SUM(bytes_in) d_in, SUM(bytes_out) d_out FROM destinations
		           WHERE ts >= ?1 AND ts < ?2 GROUP BY remote),
		     e AS (SELECT remote, SUM(bytes_in) a_in, SUM(bytes_out) a_out,
		                  GROUP_CONCAT(DISTINCT label) labels, GROUP_CONCAT(DISTINCT port) ports,
		                  MAX(dir = 'served') served
		           FROM endpoints WHERE ts >= ?1 AND ts < ?2 AND class = 'internet'%s GROUP BY remote)
		SELECT d.remote, d_in, d_out, COALESCE(a_in,0), COALESCE(a_out,0), COALESCE(labels,''),
		       COALESCE(ports,''), COALESCE(served,0),
		       COALESCE(i.ptr,''), COALESCE(i.asn,''), COALESCE(i.org,''), COALESCE(i.country,'')
		FROM d %s JOIN e USING (remote) LEFT JOIN ip_info i ON i.ip = d.remote
		ORDER BY %s DESC LIMIT ?3`, labelCond, map[bool]string{true: "", false: "LEFT"}[label != ""], order)
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var res []DestRow
	for rows.Next() {
		var r DestRow
		var served int
		if err := rows.Scan(&r.Remote, &r.In, &r.Out, &r.AttrIn, &r.AttrOut, &r.Labels, &r.Ports, &served,
			&r.PTR, &r.ASN, &r.Org, &r.Country); err != nil {
			return nil, err
		}
		r.Served = served == 1
		res = append(res, r)
	}
	return res, rows.Err()
}

type Point struct {
	TS     int64            `json:"ts"`
	Series map[string]InOut `json:"series"`
}

// Timeline buckets ground-truth internet traffic and attributed traffic by
// class (optionally for one label) into bucket-second points.
func (db *DB) Timeline(from, to, bucket int64, label string) ([]Point, error) {
	pts := map[int64]*Point{}
	get := func(ts int64) *Point {
		p := pts[ts]
		if p == nil {
			p = &Point{TS: ts, Series: map[string]InOut{}}
			pts[ts] = p
		}
		return p
	}
	scan := func(rows *sql.Rows, prefix string) error {
		defer rows.Close()
		for rows.Next() {
			var ts, in, out int64
			var k string
			if err := rows.Scan(&ts, &k, &in, &out); err != nil {
				return err
			}
			add(get(ts).Series, prefix+k, in, out)
		}
		return rows.Err()
	}
	if label == "" {
		rows, err := db.Query(`SELECT ts/?3*?3, kind, SUM(bytes_in), SUM(bytes_out) FROM `+systemUnion+
			` WHERE kind = 'internet' GROUP BY 1,2`, from, to, bucket)
		if err != nil {
			return nil, err
		}
		if err := scan(rows, "truth:"); err != nil {
			return nil, err
		}
	}
	q := `SELECT ts/?3*?3, class, SUM(bytes_in), SUM(bytes_out) FROM ` + trafficUnion
	args := []any{from, to, bucket}
	if label != "" {
		q += ` WHERE label = ?4`
		args = append(args, label)
	}
	rows, err := db.Query(q+` GROUP BY 1,2`, args...)
	if err != nil {
		return nil, err
	}
	if err := scan(rows, ""); err != nil {
		return nil, err
	}
	res := make([]Point, 0, len(pts))
	for ts := from / bucket * bucket; ts < to; ts += bucket {
		if p, ok := pts[ts]; ok {
			res = append(res, *p)
		} else {
			res = append(res, Point{TS: ts, Series: map[string]InOut{}})
		}
	}
	return res, nil
}

// Coverage reports the time span with data.
func (db *DB) Coverage() (first, last int64, err error) {
	var f, l sql.NullInt64
	err = db.QueryRow(`SELECT MIN(ts), MAX(ts) FROM (SELECT ts FROM system UNION ALL SELECT ts FROM system_hourly)`).Scan(&f, &l)
	return f.Int64, l.Int64, err
}

// ---- ip_info ------------------------------------------------------------------

type IPInfo struct {
	IP, PTR, ASN, Org, Country string
}

// IPsToLookUp returns internet endpoints seen since `since` with no (or a
// stale) ip_info row, biggest first.
func (db *DB) IPsToLookUp(since int64, staleBefore int64, limit int) ([]string, error) {
	rows, err := db.Query(`SELECT d.remote FROM destinations d LEFT JOIN ip_info i ON i.ip = d.remote
		WHERE d.ts >= ?1 AND (i.ip IS NULL OR i.looked_up < ?2)
		GROUP BY d.remote ORDER BY SUM(d.bytes_in + d.bytes_out) DESC LIMIT ?3`, since, staleBefore, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var res []string
	for rows.Next() {
		var ip string
		if err := rows.Scan(&ip); err != nil {
			return nil, err
		}
		res = append(res, ip)
	}
	return res, rows.Err()
}

func (db *DB) SaveIPInfo(i IPInfo) error {
	_, err := db.Exec(`INSERT INTO ip_info (ip,ptr,asn,org,country,looked_up) VALUES (?,?,?,?,?,?)
		ON CONFLICT(ip) DO UPDATE SET ptr=excluded.ptr, asn=excluded.asn, org=excluded.org,
		country=excluded.country, looked_up=excluded.looked_up`,
		i.IP, i.PTR, i.ASN, i.Org, i.Country, time.Now().Unix())
	return err
}
