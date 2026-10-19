package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"text/template"

	"github.com/spf13/cobra"
)

const plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.netwatch.agent</string>
    <key>ProgramArguments</key>
    <array>
        <string>{{.Binary}}</string>
        <string>start</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>{{.LogOut}}</string>
    <key>StandardErrorPath</key>
    <string>{{.LogErr}}</string>
    <key>WorkingDirectory</key>
    <string>{{.WorkDir}}</string>
</dict>
</plist>
`

var installCmd = &cobra.Command{
	Use:   "install",
	Short: "Install netwatch as a launchd agent (starts on login)",
	RunE:  runInstall,
}

var uninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove the netwatch launchd agent",
	RunE:  runUninstall,
}

func init() {
	rootCmd.AddCommand(installCmd)
	rootCmd.AddCommand(uninstallCmd)
}

func plistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", "com.netwatch.agent.plist")
}

func runInstall(cmd *cobra.Command, args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find executable: %w", err)
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return err
	}

	workDir := filepath.Dir(exe)
	logDir := filepath.Join(workDir, "logs")
	os.MkdirAll(logDir, 0755)

	data := struct {
		Binary  string
		LogOut  string
		LogErr  string
		WorkDir string
	}{
		Binary:  exe,
		LogOut:  filepath.Join(logDir, "netwatch.log"),
		LogErr:  filepath.Join(logDir, "netwatch.err"),
		WorkDir: workDir,
	}

	tmpl, err := template.New("plist").Parse(plistTemplate)
	if err != nil {
		return err
	}

	path := plistPath()
	os.MkdirAll(filepath.Dir(path), 0755)

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create plist: %w", err)
	}
	defer f.Close()

	if err := tmpl.Execute(f, data); err != nil {
		return fmt.Errorf("write plist: %w", err)
	}

	// Load the agent
	out, err := exec.Command("launchctl", "load", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl load: %s: %w", out, err)
	}

	fmt.Printf("Installed launchd agent: %s\n", path)
	fmt.Println("Netwatch will start automatically on login.")
	fmt.Printf("Dashboard: http://%s:%d\n", cfg.Server.Host, cfg.Server.Port)
	return nil
}

func runUninstall(cmd *cobra.Command, args []string) error {
	path := plistPath()

	out, err := exec.Command("launchctl", "unload", path).CombinedOutput()
	if err != nil {
		fmt.Printf("Warning: launchctl unload: %s\n", out)
	}

	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove plist: %w", err)
	}

	fmt.Println("Netwatch launchd agent removed.")
	return nil
}
