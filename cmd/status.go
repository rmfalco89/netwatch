package cmd

import (
	"fmt"
	"sort"
	"time"

	"netwatch/store"

	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Today's internet traffic and who moved it",
	RunE:  runStatus,
}

func init() {
	rootCmd.AddCommand(statusCmd)
}

func runStatus(cmd *cobra.Command, args []string) error {
	db, err := store.Open(cfg.Storage.Path)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	now := time.Now()
	y, m, d := now.Date()
	from := time.Date(y, m, d, 0, 0, 0, 0, now.Location()).Unix()
	to := now.Unix()

	sys, err := db.SystemTotals(from, to)
	if err != nil {
		return err
	}
	inet := sys["internet"]
	fmt.Printf("Internet today (router-bound, measured): %s down · %s up\n\n", humanBytes(inet.In), humanBytes(inet.Out))

	who, err := db.Who(from, to, "")
	if err != nil {
		return err
	}
	sort.Slice(who, func(i, j int) bool { return who[i].Classes["internet"].Out > who[j].Classes["internet"].Out })
	fmt.Printf("%-34s %12s %12s %12s\n", "WHO", "INET UP", "INET DOWN", "LAN UP")
	for i, r := range who {
		if i == 15 {
			break
		}
		c := r.Classes
		if c["internet"].In+c["internet"].Out+c["lan"].Out == 0 {
			continue
		}
		fmt.Printf("%-34.34s %12s %12s %12s\n", r.Name, humanBytes(c["internet"].Out), humanBytes(c["internet"].In), humanBytes(c["lan"].Out))
	}
	return nil
}

func humanBytes(b int64) string {
	const k = 1000.0
	f := float64(b)
	switch {
	case f >= k*k*k*k:
		return fmt.Sprintf("%.2f TB", f/(k*k*k*k))
	case f >= k*k*k:
		return fmt.Sprintf("%.2f GB", f/(k*k*k))
	case f >= k*k:
		return fmt.Sprintf("%.1f MB", f/(k*k))
	case f >= k:
		return fmt.Sprintf("%.1f KB", f/k)
	}
	return fmt.Sprintf("%d B", b)
}
