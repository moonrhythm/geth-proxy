package main

import (
	"context"
	"crypto/tls"
	"flag"
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/moonrhythm/parapet"
	"github.com/moonrhythm/parapet/pkg/logger"
	"github.com/moonrhythm/parapet/pkg/prom"
	"github.com/moonrhythm/parapet/pkg/upstream"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	ethClients      []*ethclient.Client
	upstreamAddrs   []string
	upstreams       []*url.URL
	muBestUpstreams sync.RWMutex
	bestUpstreams   []*url.URL
)

func main() {
	var (
		addr         = flag.String("addr", ":80", "http address")
		tlsAddr      = flag.String("tls.addr", "", "tls address")
		tlsKey       = flag.String("tls.key", "", "TLS private key file")
		tlsCert      = flag.String("tls.cert", "", "TLS certificate file")
		upstreamList = flag.String("upstream", "", "upstream list")
	)

	flag.Parse()

	log.Printf("geth-proxy")
	log.Printf("HTTP address: %s", *addr)
	log.Printf("HTTPS address: %s", *tlsAddr)
	log.Printf("Upstream: %s", *upstreamList)

	prom.Registry().MustRegister(headDuration, headNumber)

	for _, addr := range strings.Split(*upstreamList, ",") {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			continue
		}

		u, err := url.Parse(addr)
		if err != nil {
			log.Fatalf("can not parse url; %v", err)
		}

		client, err := ethclient.Dial(addr)
		if err != nil {
			log.Fatalf("can not dial geth; %v", err)
		}

		upstreamAddrs = append(upstreamAddrs, addr)
		upstreams = append(upstreams, u)
		ethClients = append(ethClients, client)
	}
	lastBlocks = make([]lastBlock, len(upstreams))
	bestUpstreams = upstreams

	go func() {
		// update stats

		for {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			updateLastBlock(ctx)
			cancel()

			time.Sleep(time.Second)
		}
	}()

	var s parapet.Middlewares

	s.Use(logger.Stdout())
	s.Use(prom.Requests())

	// http
	s.Use(upstream.New(&tr{}))

	var wg sync.WaitGroup

	if *addr != "" {
		wg.Add(1)
		srv := parapet.NewBackend()
		srv.Addr = *addr
		srv.GraceTimeout = 3 * time.Second
		srv.WaitBeforeShutdown = 0
		srv.Use(s)
		prom.Connections(srv)
		prom.Networks(srv)
		go func() {
			defer wg.Done()

			err := srv.ListenAndServe()
			if err != nil {
				log.Fatalf("can not start server; %v", err)
			}
		}()
	}

	if *tlsAddr != "" {
		wg.Add(1)
		srv := parapet.NewBackend()
		srv.Addr = *tlsAddr
		srv.GraceTimeout = 3 * time.Second
		srv.WaitBeforeShutdown = 0
		srv.TLSConfig = &tls.Config{}

		if *tlsKey == "" || *tlsCert == "" {
			cert, err := parapet.GenerateSelfSignCertificate(parapet.SelfSign{
				CommonName: "geth-proxy",
				Hosts:      []string{"geth-proxy"},
				NotBefore:  time.Now().Add(-5 * time.Minute),
				NotAfter:   time.Now().AddDate(10, 0, 0),
			})
			if err != nil {
				log.Fatalf("can not generate self signed cert; %v", err)
			}
			srv.TLSConfig.Certificates = append(srv.TLSConfig.Certificates, cert)
		} else {
			cert, err := tls.LoadX509KeyPair(*tlsCert, *tlsKey)
			if err != nil {
				log.Fatalf("can not load x509 key pair; %v", err)
			}
			srv.TLSConfig.Certificates = append(srv.TLSConfig.Certificates, cert)
		}

		srv.Use(s)
		prom.Connections(srv)
		prom.Networks(srv)
		go func() {
			defer wg.Done()

			err := srv.ListenAndServe()
			if err != nil {
				log.Fatalf("can not start server; %v", err)
			}
		}()
	}

	wg.Wait()
}

type lastBlock struct {
	mu        sync.Mutex
	Block     *types.Block
	UpdatedAt time.Time
}

var lastBlocks []lastBlock

func getLastBlock(ctx context.Context, i int) (*types.Block, error) {
	b := &lastBlocks[i]
	b.mu.Lock()
	defer b.mu.Unlock()

	if time.Since(b.UpdatedAt) < time.Second {
		return b.Block, nil
	}

	block, err := ethClients[i].BlockByNumber(ctx, nil)
	if err != nil {
		return b.Block, err
	}
	b.Block = block
	b.UpdatedAt = time.Now()
	return b.Block, nil
}

const promNamespace = "geth_proxy"

var headDuration = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: promNamespace,
	Name:      "head_duration_seconds",
}, []string{"upstream"})

var headNumber = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: promNamespace,
	Name:      "head_number",
}, []string{"upstream"})

func updateLastBlock(ctx context.Context) {
	blockNumbers := make([]uint64, len(lastBlocks))

	var wg sync.WaitGroup
	for i := range lastBlocks {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()

			block, _ := getLastBlock(ctx, i)
			if block == nil {
				return
			}

			blockNumbers[i] = block.NumberU64()

			t := time.Unix(int64(block.Time()), 0)
			diff := time.Since(t)

			g, err := headDuration.GetMetricWith(prometheus.Labels{
				"upstream": upstreamAddrs[i],
			})
			if err == nil {
				g.Set(float64(diff) / float64(time.Second))
			}

			g, err = headNumber.GetMetricWith(prometheus.Labels{
				"upstream": upstreamAddrs[i],
			})
			if err == nil {
				g.Set(float64(block.NumberU64()))
			}
		}()
	}
	wg.Wait()

	h := highestBlock(blockNumbers)

	// collect all best block rpc
	best := make([]*url.URL, 0, len(blockNumbers))
	for i, b := range blockNumbers {
		if b == h {
			best = append(best, upstreams[i])
		}
	}

	muBestUpstreams.Lock()
	bestUpstreams = best
	muBestUpstreams.Unlock()
}

func highestBlock(blockNumbers []uint64) uint64 {
	max := blockNumbers[0]
	for _, x := range blockNumbers {
		if x > max {
			max = x
		}
	}
	return max
}

type tr struct {
	i uint32
}

var (
	trs = map[string]http.RoundTripper{
		"http": &upstream.HTTPTransport{
			MaxIdleConns: 10000,
		},
		"https": &upstream.HTTPSTransport{
			MaxIdleConns: 10000,
		},
	}
)

// RoundTrip sends a request to upstream server
func (tr *tr) RoundTrip(r *http.Request) (*http.Response, error) {
	muBestUpstreams.RLock()
	targets := bestUpstreams
	muBestUpstreams.RUnlock()

	if len(targets) == 0 {
		return nil, upstream.ErrUnavailable
	}

	i := atomic.AddUint32(&tr.i, 1) - 1
	i %= uint32(len(targets))
	t := targets[i]

	r.URL.Scheme = t.Scheme
	r.URL.Host = t.Host
	r.URL.Path = path.Join(t.Path, r.URL.Path)
	r.Host = t.Host
	log.Println(r.URL.String())
	return trs[t.Scheme].RoundTrip(r)
}
