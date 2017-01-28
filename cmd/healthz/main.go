package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/gravitational/trace"

	pb "github.com/gravitational/satellite/agent/proto/agentpb"
	"github.com/gravitational/satellite/healthz/checks"
	"github.com/gravitational/satellite/healthz/config"
	"github.com/gravitational/satellite/healthz/handlers"
	"github.com/gravitational/satellite/healthz/utils"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
		fmt.Printf("ERROR: %v\n", err.Error())
		os.Exit(255)
	}
}

func run() error {
	cfg := config.Config{}
	config.ParseCLIFlags(&cfg)

	if cfg.Debug {
		trace.EnableDebug()
		log.SetLevel(log.DebugLevel)
	}
	log.SetFormatter(&log.TextFormatter{})
	log.SetOutput(os.Stderr)

	log.Debug(trace.Errorf("starting using config: %#v", cfg))

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		exitSignals := make(chan os.Signal, 1)
		signal.Ignore()
		signal.Notify(exitSignals, syscall.SIGTERM, syscall.SIGINT)

		select {
		case sig := <-exitSignals:
			log.Infof("signal: %v", sig)
			cancel()
		}
	}()

	errChan := make(chan error, 10)
	clusterHealth := &pb.Probe{
		Status: pb.Probe_Running,
		Error:  reasonNoChecksYet,
	}
	clusterHealthMu := sync.Mutex{}

	http.HandleFunc("/healthz", func(w http.ResponseWriter, req *http.Request) {
		log.Infof("%s %s %s %s", req.RemoteAddr, req.Host, req.RequestURI, req.UserAgent())
		if !handlers.Auth(cfg.AccessKey, w, req) {
			return
		}
		clusterHealthMu.Lock()
		status := *clusterHealth
		clusterHealthMu.Unlock()
		handlers.Healthz(status, w, req)
	})

	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return trace.Wrap(err)
	}

	if cfg.CAFile != "" && cfg.KeyFile != "" && cfg.CertFile != "" {
		tlsConfig, err := utils.NewServerTLS(cfg.CertFile, cfg.KeyFile, cfg.CAFile)
		if err != nil {
			return trace.Wrap(err)
		}
		listener = tls.NewListener(listener, tlsConfig)
	}

	go func() {
		if err := http.Serve(listener, nil); err != nil {
			errChan <- trace.Wrap(err)
			return
		}
	}()

	go func() {
		for {
			ticker := time.NewTimer(cfg.CheckInterval)
			defer ticker.Stop()
			select {
			case <-ticker.C:
				status, err := checks.RunAll(cfg)
				if err != nil {
					errChan <- trace.Wrap(err)
					return
				}
				clusterHealthMu.Lock()
				clusterHealth = status
				clusterHealthMu.Unlock()
			}
		}
	}()

	select {
	case err := <-errChan:
		return err
	case <-ctx.Done():
		return nil
	}
}

const reasonNoChecksYet = "No checks ran yet"
