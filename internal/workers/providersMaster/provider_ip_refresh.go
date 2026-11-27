package providersmaster

import (
	"context"
	"encoding/hex"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"mytonprovider-backend/internal/models/db"
)

var (
	providerIPRefreshMetricsOnce sync.Once

	providerIPRefreshAttemptsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "ton_storage",
			Subsystem: "mtpo_workers",
			Name:      "provider_ip_refresh_attempts_total",
			Help:      "Provider ADNL IP refresh attempts grouped by result",
		},
		[]string{"result"},
	)
)

func initProviderIPRefreshMetrics() {
	providerIPRefreshMetricsOnce.Do(func() {
		prometheus.MustRegister(providerIPRefreshAttemptsTotal)
	})
}

func (w *providersMasterWorker) RefreshProviderIPs(ctx context.Context) (interval time.Duration, err error) {
	successInterval := w.providerIPCfg.Interval
	if successInterval <= 0 {
		successInterval = defaultProviderIPRefreshInterval
	}

	log := w.logger.With(slog.String("worker", "RefreshProviderIPs"))
	interval = successInterval

	pubkeys, err := w.providers.GetAllProvidersPubkeys(ctx)
	if err != nil {
		log.Error("failed to list providers for IP refresh", "error", err)
		return providerIPRefreshFailInterval, err
	}
	if len(pubkeys) == 0 {
		log.Info("no providers for IP refresh")
		return successInterval, nil
	}

	resolved := make([]db.ProviderIP, 0, len(pubkeys))
	semaphore := make(chan struct{}, maxConcurrentProviderChecks)
	var wg sync.WaitGroup
	var mu sync.Mutex

	for _, pubkey := range pubkeys {
		if ctx.Err() != nil {
			break
		}

		wg.Add(1)
		go func(pubkey string) {
			defer wg.Done()

			select {
			case <-ctx.Done():
				return
			case semaphore <- struct{}{}:
			}
			defer func() { <-semaphore }()

			if ctx.Err() != nil {
				return
			}

			providerIPRefreshAttemptsTotal.WithLabelValues("attempt").Inc()

			raw, decErr := hex.DecodeString(pubkey)
			if decErr != nil {
				providerIPRefreshAttemptsTotal.WithLabelValues("error").Inc()
				log.Error("failed to decode provider public key", "provider_pubkey", pubkey, "error", decErr)
				return
			}

			ip, findErr := w.findProviderIP(ctx, raw)
			if findErr != nil {
				providerIPRefreshAttemptsTotal.WithLabelValues("error").Inc()
				log.Debug("provider IP not resolved", "provider_pubkey", pubkey, "error", findErr)
				return
			}
			if strings.TrimSpace(ip.IP) == "" {
				providerIPRefreshAttemptsTotal.WithLabelValues("error").Inc()
				log.Debug("provider IP is empty", "provider_pubkey", pubkey)
				return
			}

			providerIPRefreshAttemptsTotal.WithLabelValues("success").Inc()
			row := db.ProviderIP{
				PublicKey: pubkey,
				Provider:  ip,
			}

			mu.Lock()
			resolved = append(resolved, row)
			mu.Unlock()

			w.patchLastKnownProviderIP(pubkey, ip)
		}(pubkey)
	}
	wg.Wait()

	if len(resolved) > 0 {
		if err = w.providers.UpdateProviderADIPs(ctx, resolved); err != nil {
			log.Error("failed to persist provider IPs", "error", err)
			return providerIPRefreshFailInterval, err
		}
	}

	if ctx.Err() != nil {
		return providerIPRefreshFailInterval, ctx.Err()
	}

	log.Info("provider IP refresh completed", "providers", len(pubkeys), "resolved", len(resolved))
	return successInterval, nil
}

func (w *providersMasterWorker) patchLastKnownProviderIP(pubkey string, ip db.IPInfo) {
	w.lastIPsMu.Lock()
	defer w.lastIPsMu.Unlock()

	known, ok := w.lastKnownIPs[pubkey]
	if !ok {
		return
	}
	known.Provider = ip
	w.lastKnownIPs[pubkey] = known
}
