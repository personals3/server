package livefeed

import (
	"context"
	"math"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/personals3/api/internal/storage"
)

// StartStats publishes a stats event every 5s: real disk usage (statfs on
// the storage root), the rolling req/min counter, and the count of
// transcode jobs currently being worked.
func (b *Broker) StartStats(ctx context.Context, db *pgxpool.Pool, storageRoot string) {
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				b.publishStats(ctx, db, storageRoot)
			}
		}
	}()
}

func (b *Broker) publishStats(ctx context.Context, db *pgxpool.Pool, storageRoot string) {
	diskPct := 0.0
	if ds, err := storage.Stat(storageRoot); err == nil && ds.Total > 0 {
		diskPct = float64(ds.Used) / float64(ds.Total) * 100
	}

	var active int
	qctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	_ = db.QueryRow(qctx,
		`SELECT COUNT(*) FROM transcode_jobs WHERE status = 'processing'`,
	).Scan(&active)
	cancel()

	b.publish(StatsEvent{
		Type:             "stats",
		DiskUsedPct:      math.Round(diskPct*10) / 10,
		ReqPerMin:        b.reqPerMin(),
		ActiveTranscodes: active,
		UptimeS:          int64(time.Since(b.started).Seconds()),
		TS:               time.Now().UnixMilli(),
	})
}
