package cmd

import (
	"context"
	"fmt"
	"log"
	"netwatch/api"
	"netwatch/collector"
	"netwatch/store"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the netwatch daemon (foreground)",
	RunE:  runStart,
}

func init() {
	rootCmd.AddCommand(startCmd)
}

func runStart(cmd *cobra.Command, args []string) error {
	db, err := store.Open(cfg.Storage.Path)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	c := collector.New(db, cfg)
	c.Start()

	srv := api.NewServer(db, cfg, c)
	go func() {
		addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
		log.Printf("[server] listening on http://%s", addr)
		if err := srv.ListenAndServe(addr); err != nil {
			log.Printf("[server] error: %v", err)
		}
	}()

	// Wait for interrupt
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	log.Println("[netwatch] shutting down...")
	c.Stop()
	srv.Shutdown()

	// Write PID file removal if it exists
	pidFile := pidFilePath()
	os.Remove(pidFile)

	return nil
}

func pidFilePath() string {
	return cfg.Storage.Path + ".pid"
}
