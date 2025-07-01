package main

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"github.com/jetclock/jetclock-sdk/pkg/config"
	"github.com/jetclock/jetclock-sdk/pkg/hotspot"
	"github.com/jetclock/jetclock-sdk/pkg/logger"
	"github.com/jetclock/jetclock-sdk/pkg/statusimg"
	"github.com/jetclock/jetclock-sdk/pkg/update"
	"github.com/jetclock/jetclock-sdk/pkg/utils"
	"github.com/jetclock/jetclock-sdk/pkg/wifi"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"
)

//go:embed update.png
var updatePng []byte

const repository = "jetclock/jetclock"
const binary = "/home/jetclock/.jetclock/jetclock"

var updateFinished atomic.Bool // true when updateProcess() returns
var inHotspot atomic.Bool      // set true right after hotspot.Start() succeeds

func main() {
	if err := logger.InitLogger(logger.LogToFile|logger.LogToStdout, filepath.Join("/home", "jetclock")); err != nil {
		log.Fatalf("Failed to init logger: %v", err)
	}

	// -------------------------------------------------------------------
	// Signal handling  (Ctrl-C / SIGTERM / SIGUSR1)
	// -------------------------------------------------------------------
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh,
		os.Interrupt,    // == SIGINT (Ctrl-C)
		syscall.SIGTERM, // graceful stop
		syscall.SIGUSR1, // JetClock DOM ready
	)

	go func() {
		for sig := range sigCh {
			switch sig {

			// JetClock tells us its UI is ready
			case syscall.SIGUSR1:
				if updateFinished.Load() && !inHotspot.Load() {
					statusimg.Stop() // hide splash now
					logger.Log.Info("🖼️  Splash cleared on SIGUSR1")
				}

			// Any “quit” signal → tidy up and exit
			case os.Interrupt, syscall.SIGTERM:
				statusimg.Stop() // kill feh, rm pidfile
				os.Exit(0)
			}
		}
	}()

	// **always** clean up when main() returns:**
	defer func() {
		if err := statusimg.Stop(); err != nil && !os.IsNotExist(err) {
			log.Printf("failed to stop splash: %v", err)
		}
	}()

	// Show the *update* splash first
	err := statusimg.Show(statusimg.UpdateStarting)
	if err != nil {
		logger.Log.Errorf("could not show update image %v", err)
	}
	defer statusimg.Stop()
	logger.Log.Info("image should be showing now")

	logger.Log.Info("config location is " + filepath.Join("/home", "jetclock", ".config", "jetclock", "config.yaml"))
	appConfig, err := config.LoadConfig(filepath.Join("/home", "jetclock", ".config", "jetclock", "config.yaml"))
	if err != nil {
		logger.Log.Infof("could not load config: %v", err)
	}
	logger.Log.Infof("config is %+v\n", appConfig)
	// 1) figure out current version
	var version string
	if out, err := exec.Command(binary, "--version").Output(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to get local version: %v\n", err)
		version = "v0.0.1"
	} else {
		version = string(bytes.TrimSpace(out))
	}

	logger.Log.Info("Starting jetclock update process - received version", "version", version)

	config := hotspot.DefaultConfig //todo tidy this up. Too many configs...
	if os.Getenv("JETCLOCK_PORT") != "" {
		config.Port = os.Getenv("JETCLOCK_PORT")
	} else {
		config.Port = "80" //hardcode this version to 80 for the pi
	}
	internetOK := false

	if !appConfig.ForceHotspot {
		// Fast path: already online?
		if wifi.HasInternet() { // HasInternet == VerifyInternet
			internetOK = true
			logger.Log.Info("✅ Existing Internet connection detected")
		} else {
			// Try once to re-connect to any known network (20-s budget)
			logger.Log.Info("📶 Attempting to connect to a known Wi-Fi network")
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			if err := wifi.Connect(ctx); err == nil { // Connect() includes VerifyInternet
				internetOK = true
				logger.Log.Info("✅ Reconnected successfully")
			} else {
				logger.Log.Warn("⚠️  Reconnect failed:", "error", err)
			}
			cancel() // free timer regardless of outcome
		}
	}

	// ----------------------------------------------------------------------
	// 1️⃣  Act on the result
	// ----------------------------------------------------------------------
	if internetOK {
		logger.Log.Info("🔄 Running update process")
		go updateProcess(version, appConfig.PreRelease)
		// Internet is live: ensure hotspot is down in case it was left over
		hotspot.StopHotspot()
	} else {
		logger.Log.Info("🚫 No Internet — starting hotspot")

		statusimg.Show(statusimg.HotspotPNG) // Hotspot running
		if err != nil {
			logger.Log.Errorf("could not show hotspot image %v", err)
		}
		hotspot.StopHotspot() // clean slate
		if err := hotspot.Start(config); err != nil {
			logger.Log.Error("❌ Failed to start hotspot:", "error", err)
		} else {
			inHotspot.Store(true)
		}
	}

	// ----------------------------------------------------------------------
	// 2️⃣  In every case, serve the captive-portal UI
	// ----------------------------------------------------------------------
	server, err := hotspot.NewServer(config)
	if err != nil {
		log.Fatalf("Failed to create portal server: %v", err)
	}
	/* ----------  NEW  --------------------------------------------------- */
	// After 45 s: hide whatever splash image is still up.
	time.AfterFunc(45*time.Second, func() {
		statusimg.Stop()
		logger.Log.Info("🖼️  Splash image cleared after 45s")
	})
	// After 5 min: stop splash (just in case) and exit the updater.
	// That lets JetClock (or whatever systemd service) restart the main app.
	time.AfterFunc(5*time.Minute, func() {
		logger.Log.Info("⏱  5-minute window elapsed — shutting down updater")
		statusimg.Stop()
		os.Exit(0)
	})
	server.Start() // blocking
}

func updateProcess(version string, preRelease bool) {
	if updated, err := update.AutoUpdateCommand(binary, version, repository, preRelease); err != nil {
		fmt.Println("Failed to check update:", err)
		logger.Log.Errorf("Update failed: %v", err)
	} else if updated {
		fmt.Println("Update succeeded")
		logger.Log.Info("✅ Update applied — rebooting")
		//todo show a rebooting image as the update succeeded.
		go utils.RunCommandOnly("sudo", "reboot")

	}
	updateFinished.Store(true)
	err := statusimg.Show(statusimg.UpdateComplete)
	if err != nil {
		logger.Log.Errorf("could not show hotspot image %v", err)
	}
	return
}
