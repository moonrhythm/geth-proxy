package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/moonrhythm/parapet"
	"github.com/moonrhythm/parapet/pkg/location"
	"github.com/moonrhythm/parapet/pkg/logger"
	"github.com/moonrhythm/parapet/pkg/stripprefix"
	"github.com/moonrhythm/parapet/pkg/upstream"
)

var (
	ethClient       *ethclient.Client
	blockDuration   time.Duration
	healthyDuration time.Duration
)

func main() {
	var (
		addr                = flag.String("addr", ":80", "address to listening")
		gethHTTP            = flag.String("geth.http", "127.0.0.1:8545", "geth http port")
		gethWS              = flag.String("geth.ws", "127.0.0.1:8546", "geth ws port")
		gethMetrics         = flag.String("geth.metics", "127.0.0.1:6060", "geth metrics port")
		gethBlockDuration   = flag.Duration("geth.block-duration", time.Second, "block duration (default: 1 second)")
		gethHealthzDuration = flag.Duration("geth.healthy.duration", 30*time.Second, "duration from last block that mark as healthy")
	)

	flag.Parse()

	// TODO: lazy dial ?
	var err error
	ethClient, err = ethclient.Dial(*gethHTTP)
	if err != nil {
		log.Fatalf("can not dial geth; %v", err)
	}
	blockDuration = *gethBlockDuration
	healthyDuration = *gethHealthzDuration

	srv := parapet.NewBackend()
	srv.Addr = *addr

	srv.Use(logger.Stdout())

	// healthz
	{
		l := location.Exact("/healthz")
		l.Use(parapet.Handler(healthz))
		srv.Use(l)
	}

	// websocket
	if *gethWS != "" {
		l := location.Exact("/ws")
		l.Use(stripprefix.New("/ws"))
		l.Use(upstream.SingleHost(*gethWS, &upstream.HTTPTransport{}))
		srv.Use(l)
	}

	// metrics
	if *gethMetrics != "" {
		l := location.Prefix("/debug/")
		l.Use(upstream.SingleHost(*gethMetrics, &upstream.HTTPTransport{}))
		srv.Use(l)
	}

	// http
	srv.Use(upstream.SingleHost(*gethHTTP, &upstream.HTTPTransport{
		MaxIdleConns: 10000,
	}))
	srv.ListenAndServe()
}

func isReady(ctx context.Context) (bool, error) {
	block, err := ethClient.BlockByNumber(ctx, nil)
	if err != nil {
		return false, err
	}
	ts := block.Time()
	ts = ts * uint64(blockDuration) // convert to ns
	t := time.Unix(0, int64(ts))
	return time.Since(t) < healthyDuration, nil
}

func isLive(ctx context.Context) bool {
	_, err := ethClient.BlockByNumber(ctx, nil)
	return err == nil
}

func healthz(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.FormValue("ready") == "1" {
		ready, err := isReady(ctx)
		if err != nil {
			http.Error(w, "can not check last block status", http.StatusInternalServerError)
			return
		}
		if !ready {
			http.Error(w, "not ready", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ready"))
		return
	}

	live := isLive(ctx)
	if !live {
		http.Error(w, "not ok", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}
