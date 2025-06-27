package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"github.com/jetclock/jetclock-sdk/pkg/hotspot"
	"github.com/jetclock/jetclock-sdk/pkg/logger"
	"github.com/jetclock/jetclock-sdk/pkg/update"
	"github.com/jetclock/jetclock-sdk/pkg/utils"
	"github.com/jetclock/jetclock-sdk/pkg/wifi"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

//go:embed update.png
var updatePng []byte

const repository = "jetclock/jetclock"
const binary = "/home/jetclock/.jetclock/jetclock"

// imageInit writes the embedded update.png to destPath if it doesn't already exist.
// It will create any parent directories as needed.
func imageInit(destPath string) error {
	// Check if file already exists
	if _, err := os.Stat(destPath); err == nil {
		// already there
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("unable to stat %q: %w", destPath, err)
	}

	// Make sure parent directory exists
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("failed to create dirs for %q: %w", destPath, err)
	}

	// Write the embedded PNG out
	if err := os.WriteFile(destPath, updatePng, 0o644); err != nil {
		return fmt.Errorf("failed to write %q: %w", destPath, err)
	}
	fmt.Println("wrote the image to ", destPath)
	return nil
}

func showImage(path, pidFile string) (int, error) {
	fehCmd := exec.Command("feh",
		"--fullscreen",
		"--hide-pointer",
		"--auto-zoom",
		path,
	)
	fmt.Println("about to show image")
	if err := fehCmd.Start(); err != nil {
		return 0, fmt.Errorf("starting feh: %w", err)
	}
	pid := fehCmd.Process.Pid
	// Save PID for later cleanup
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(pid)), 0644); err != nil {
		return 0, fmt.Errorf("writing pidfile: %w", err)
	}
	fmt.Println("image was started with pid ", pid)
	return pid, nil
}

// cleanupSplash sends SIGTERM then SIGKILL to feh, and removes pidfile
func cleanupSplash(pid int, pidFile string) {
	// polite
	syscall.Kill(pid, syscall.SIGTERM)
	// ensure it's gone
	syscall.Kill(pid, syscall.SIGKILL)
	if err := os.Remove(pidFile); err != nil && !os.IsNotExist(err) {
		log.Printf("warning: could not remove pidfile: %v", err)
	}
}
func stopImage(pidFile string) error {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return err
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		return err
	}
	if err := exec.Command("kill", strconv.Itoa(pid)).Run(); err != nil {
		return err
	}
	return os.Remove(pidFile)
}
func main() {
	preRelease := true
	if err := logger.InitLogger(logger.LogToFile | logger.LogToStdout); err != nil {
		log.Fatalf("Failed to init logger: %v", err)
	}
	pidFile := "/tmp/feh.pid"
	out := "/home/jetclock/images/update.png"
	if err := imageInit(out); err != nil {
		fmt.Fprintf(os.Stderr, "imageInit error: %v\n", err)
		os.Exit(1)
	}
	// **always** clean up when main() returns:**
	defer func() {
		if err := stopImage(pidFile); err != nil && !os.IsNotExist(err) {
			log.Printf("failed to stop splash: %v", err)
		}
	}()

	if pid, err := showImage(out, pidFile); err != nil {
		panic(err)
	} else {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-sigs
			cleanupSplash(pid, pidFile)
			os.Exit(0)
		}()
	}
	logger.Log.Info("image should be showing now")

	// 1) figure out current version
	var version string
	if out, err := exec.Command(binary, "--version").Output(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to get local version: %v\n", err)
		version = "v0.0.1"
	} else {
		version = string(bytes.TrimSpace(out))
	}

	logger.Log.Info("Starting jetclock update process - received version", "version", version)

	// If has a wifi (need to check for internet)s
	if wifi.IsConnected() {
		logger.Log.Info("✅ Wi-Fi OK — checking for update of", "binary", binary)
		updateProcess(version, preRelease)
		return
	}

	// Not connected: try to connect
	logger.Log.Info("📶 Not on Wi-Fi — attempting to connect to known network")
	if err := wifi.Connect(); err == nil {
		logger.Log.Info("✅ Connected — now checking for update of", "binary", binary)
		updateProcess(version, preRelease)
		return
	} else {
		logger.Log.Warn("⚠️  Connect failed, falling back to hotspot:", "error", err)
	}

	cfg := hotspot.DefaultConfig

	// If we reach here, connect failed → start hotspot + captive-portal
	if err := hotspot.StopHotspot(); err != nil {
		logger.Log.Warn("Failed to clean up old hotspot:", "error", err)
	}
	hotspot.Start(cfg)

	// serve the portal (blocking)
	server, err := hotspot.NewServer(cfg)
	if err != nil {
		log.Fatalf("Failed to create portal server: %v", err)
	}
	server.Start()
}

func updateProcess(version string, preRelease bool) {
	if updated, err := update.AutoUpdateCommand(binary, version, repository, preRelease); err != nil {
		fmt.Println("Failed to check update:", err)
		logger.Log.Errorf("Update failed: %v", err)
	} else if updated {
		fmt.Println("Update succeeded")
		logger.Log.Info("✅ Update applied — rebooting")
		go utils.RunCommandOnly("sudo", "reboot")
	}
	//the app can be killed externally or it will just stay open for 45 seconds. Whichever happens first
	time.Sleep(45 * time.Second)
	return
}
