package cmd

import (
	"fmt"
	"netwatch/config"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var cfg *config.Config

var rootCmd = &cobra.Command{
	Use:   "netwatch",
	Short: "macOS per-process network usage monitor",
	Long:  "Netwatch monitors and records per-process network usage on macOS.",
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func init() {
	cobra.OnInitialize(initConfig)
	rootCmd.PersistentFlags().StringP("config", "c", "", "config file path")
}

func initConfig() {
	cfg = config.Default()

	cfgFile, _ := rootCmd.Flags().GetString("config")
	if cfgFile != "" {
		viper.SetConfigFile(cfgFile)
	} else {
		// Look for config in the binary's directory and working directory
		exe, _ := os.Executable()
		if exe != "" {
			viper.AddConfigPath(filepath.Dir(exe))
		}
		viper.AddConfigPath(".")
		viper.SetConfigName("netwatch")
		viper.SetConfigType("yaml")
	}

	viper.AutomaticEnv()
	viper.SetEnvPrefix("NETWATCH")

	if err := viper.ReadInConfig(); err == nil {
		fmt.Fprintln(os.Stderr, "Using config:", viper.ConfigFileUsed())
	}

	if err := viper.Unmarshal(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "Error parsing config:", err)
	}
}
