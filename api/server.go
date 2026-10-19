package api

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"netwatch/collector"
	"netwatch/config"
	"netwatch/store"
)

//go:embed all:static
var staticFiles embed.FS

type Server struct {
	db        *store.DB
	cfg       *config.Config
	collector *collector.Collector
	srv       *http.Server
}

func NewServer(db *store.DB, cfg *config.Config, c *collector.Collector) *Server {
	return &Server{db: db, cfg: cfg, collector: c}
}

// The API is read-only: there is nothing to change through it.
func (s *Server) ListenAndServe(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/summary", s.handleSummary)
	mux.HandleFunc("GET /api/who", s.handleWho)
	mux.HandleFunc("GET /api/where", s.handleWhere)
	mux.HandleFunc("GET /api/destinations", s.handleDestinations)
	mux.HandleFunc("GET /api/timeline", s.handleTimeline)
	mux.HandleFunc("GET /api/status", s.handleStatus)

	staticSub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return err
	}
	mux.Handle("GET /", http.FileServer(http.FS(staticSub)))

	s.srv = &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	return s.srv.ListenAndServe()
}

func (s *Server) Shutdown() {
	if s.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.srv.Shutdown(ctx)
	}
}

// Summary is the answer to "how much really went over the network, and how
// much of it can be pinned on someone".
type Summary struct {
	From int64 `json:"from"`
	To   int64 `json:"to"`
	// Ground truth.
	Internet   store.InOut            `json:"internet"`   // default routes: left via the router
	Interfaces map[string]store.InOut `json:"interfaces"` // en0, en1, utun*, vmenet*, lo0
	Wire       store.InOut            `json:"wire"`       // sum of physical en* interfaces
	LAN        store.InOut            `json:"lan"`        // wire minus internet
	UTM        store.InOut            `json:"utm"`        // vmenet* interfaces: UTM VMs, from the VMs' side
	// Attributed to processes, by class (internet, lan, vpn, tailnet,
	// unclassified, host).
	Classes map[string]store.InOut `json:"classes"`
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	from, to := parseRange(r)
	sys, err := s.db.SystemTotals(from, to)
	if err != nil {
		httpError(w, err)
		return
	}
	classes, err := s.db.ClassTotals(from, to, "")
	if err != nil {
		httpError(w, err)
		return
	}
	sum := Summary{From: from, To: to, Internet: sys["internet"], Interfaces: map[string]store.InOut{}, Classes: classes}
	for k, v := range sys {
		name, ok := strings.CutPrefix(k, "if:")
		if !ok {
			continue
		}
		sum.Interfaces[name] = v
		switch {
		case strings.HasPrefix(name, "en"):
			sum.Wire.In += v.In
			sum.Wire.Out += v.Out
		case strings.HasPrefix(name, "vmenet"):
			// The Mac receives what the VM sends: flip to the VM's view.
			sum.UTM.In += v.Out
			sum.UTM.Out += v.In
		}
	}
	sum.LAN = store.InOut{In: max(0, sum.Wire.In-sum.Internet.In), Out: max(0, sum.Wire.Out-sum.Internet.Out)}
	jsonResponse(w, sum)
}

func (s *Server) handleWho(w http.ResponseWriter, r *http.Request) {
	from, to := parseRange(r)
	rows, err := s.db.Who(from, to, r.URL.Query().Get("label"))
	if err != nil {
		httpError(w, err)
		return
	}
	jsonResponse(w, rows)
}

func (s *Server) handleWhere(w http.ResponseWriter, r *http.Request) {
	from, to := parseRange(r)
	q := r.URL.Query()
	rows, err := s.db.Where(from, to, q.Get("label"), q.Get("class"), q.Get("sort"), parseInt(q.Get("limit"), 100))
	if err != nil {
		httpError(w, err)
		return
	}
	jsonResponse(w, rows)
}

func (s *Server) handleDestinations(w http.ResponseWriter, r *http.Request) {
	from, to := parseRange(r)
	q := r.URL.Query()
	rows, err := s.db.Destinations(from, to, q.Get("label"), q.Get("sort"), parseInt(q.Get("limit"), 100))
	if err != nil {
		httpError(w, err)
		return
	}
	jsonResponse(w, rows)
}

func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request) {
	from, to := parseRange(r)
	bucket := int64(parseInt(r.URL.Query().Get("bucket"), 0))
	if bucket <= 0 {
		bucket = autoBucket(to - from)
	}
	pts, err := s.db.Timeline(from, to, bucket, r.URL.Query().Get("label"))
	if err != nil {
		httpError(w, err)
		return
	}
	jsonResponse(w, map[string]any{"bucket": bucket, "points": pts})
}

// autoBucket keeps a chart around 100-300 points. Hourly-rolled data is
// never finer than an hour, so long ranges use whole hours or days.
func autoBucket(span int64) int64 {
	switch {
	case span <= 2*3600:
		return 60
	case span <= 12*3600:
		return 300
	case span <= 2*86400:
		return 900
	case span <= 8*86400:
		return 3600
	default:
		return 86400
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	st := s.collector.Stats()
	first, last, _ := s.db.Coverage()
	healthy := time.Since(st.LastExternal) < 2*time.Minute && time.Since(st.LastLoopback) < 2*time.Minute
	if st.Flushes > 0 && st.LastFlushError != "" {
		healthy = false
	}
	if !healthy && time.Since(st.Started) > 2*time.Minute {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	jsonResponse(w, map[string]any{
		"healthy":         healthy,
		"collector":       st,
		"data_from":       first,
		"data_to":         last,
		"stream_interval": s.cfg.Collector.StreamInterval,
		"flush_interval":  s.cfg.Collector.FlushInterval,
	})
}

// parseRange accepts from/to (unix seconds) or period: "today" (since local
// midnight), or a duration such as 1h, 24h, 7d, 30d. Default: today.
func parseRange(r *http.Request) (int64, int64) {
	now := time.Now()
	q := r.URL.Query()
	to := int64(parseInt(q.Get("to"), int(now.Unix())))
	if f := q.Get("from"); f != "" {
		return int64(parseInt(f, 0)), to
	}
	period := q.Get("period")
	if period == "" || period == "today" {
		y, m, d := now.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, now.Location()).Unix(), to
	}
	if days, ok := strings.CutSuffix(period, "d"); ok {
		if n, err := strconv.Atoi(days); err == nil {
			return now.Add(-time.Duration(n) * 24 * time.Hour).Unix(), to
		}
	}
	if d, err := time.ParseDuration(period); err == nil {
		return now.Add(-d).Unix(), to
	}
	return now.Add(-24 * time.Hour).Unix(), to
}

func parseInt(s string, def int) int {
	if v, err := strconv.Atoi(s); err == nil {
		return v
	}
	return def
}

func jsonResponse(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		log.Printf("[api] json encode error: %v", err)
	}
}

func httpError(w http.ResponseWriter, err error) {
	log.Printf("[api] error: %v", err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
