package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Delivery is one row of the deliveries table.
type Delivery struct {
	// ID is the delivery ID.
	ID int64
	// IncidentID is the incident the delivery belongs to; nil for a digest or
	// a test.
	IncidentID *int64
	// Kind is what the delivery carries.
	Kind DeliveryKind
	// Channel is the channel the message is delivered through.
	Channel string
	// CreatedAt is when the delivery was created.
	CreatedAt time.Time
	// SentAt is when the delivery succeeded; nil while it is pending or
	// failed.
	SentAt *time.Time
	// Attempts is the number of attempts made.
	Attempts int
	// Status is pending, sent, failed, or the reason the paging gate did not
	// page: silenced, acked, held or cooldown.
	Status string
	// Error is the last error message; nil when there is none.
	Error *string
	// TelegramChatID and TelegramMessageID identify the Telegram message that
	// delivered this row. The inbox uses the pair to resolve reply-to
	// messages; message ids are only unique within one chat.
	TelegramChatID    *int64
	TelegramMessageID *int64
}

// deliveryColumns is the SELECT list scanDelivery reads.
const deliveryColumns = `d.id, d.incident_id, d.kind, d.channel, d.created_at, d.sent_at, d.attempts,
	d.status, d.error, d.telegram_chat_id, d.telegram_message_id`

// scanDelivery reads one row selected with deliveryColumns into d, followed
// by any extra columns.
func scanDelivery(row pgx.Row, d *Delivery, extra ...any) error {
	fields := append([]any{&d.ID, &d.IncidentID, &d.Kind, &d.Channel, &d.CreatedAt, &d.SentAt,
		&d.Attempts, &d.Status, &d.Error, &d.TelegramChatID, &d.TelegramMessageID}, extra...)
	return row.Scan(fields...)
}

// AddDelivery records a new delivery and returns its ID.
func (s *Store) AddDelivery(ctx context.Context, delivery Delivery) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		insert into deliveries (incident_id, kind, channel, created_at, sent_at, attempts, status, error,
		                        telegram_chat_id, telegram_message_id)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		returning id`,
		delivery.IncidentID, delivery.Kind, delivery.Channel, delivery.CreatedAt, delivery.SentAt,
		delivery.Attempts, delivery.Status, delivery.Error, delivery.TelegramChatID,
		delivery.TelegramMessageID).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert delivery: %w", err)
	}
	return id, nil
}

// UpdateDelivery records the outcome of a delivery attempt and, when the
// delivery succeeded via Telegram, the chat and message_id Telegram returned.
func (s *Store) UpdateDelivery(ctx context.Context, delivery Delivery) error {
	_, err := s.pool.Exec(ctx, `
		update deliveries
		set sent_at = $2, attempts = $3, status = $4, error = $5, telegram_chat_id = $6, telegram_message_id = $7
		where id = $1`,
		delivery.ID, delivery.SentAt, delivery.Attempts, delivery.Status, delivery.Error,
		delivery.TelegramChatID, delivery.TelegramMessageID)
	if err != nil {
		return fmt.Errorf("update delivery %d: %w", delivery.ID, err)
	}
	return nil
}

// FailPendingDeliveries marks every delivery still pending as failed, with
// reason as its error, and returns how many it changed. A pending delivery
// waits for a retry that lives in memory, so after a restart nothing will
// ever finish it.
func (s *Store) FailPendingDeliveries(ctx context.Context, reason string) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		update deliveries
		set status = 'failed', error = coalesce(error || '; ', '') || $1
		where status = 'pending'`, reason)
	if err != nil {
		return 0, fmt.Errorf("fail pending deliveries: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Deliveries returns the deliveries of one incident, newest first.
func (s *Store) Deliveries(ctx context.Context, incidentID int64) ([]Delivery, error) {
	rows, err := s.pool.Query(ctx, `select `+deliveryColumns+`
		from deliveries d where d.incident_id = $1 order by d.created_at desc, d.id desc`, incidentID)
	if err != nil {
		return nil, fmt.Errorf("read deliveries: %w", err)
	}
	defer rows.Close()
	var deliveries []Delivery
	for rows.Next() {
		var delivery Delivery
		if err := scanDelivery(rows, &delivery); err != nil {
			return nil, fmt.Errorf("scan delivery: %w", err)
		}
		deliveries = append(deliveries, delivery)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read deliveries: %w", err)
	}
	return deliveries, nil
}

// AppDayStats is one app's request and latency data over a day.
type AppDayStats struct {
	// AppID is the app.
	AppID int64
	// Name is the app name.
	Name string
	// Requests counts the requests.
	Requests int64
	// S5xx counts the failing responses.
	S5xx int64
	// Latency is the summed latency histogram.
	Latency Histogram
}

// TelegramMessageDeliveries returns the deliveries carried by one Telegram
// message, oldest first. A reply to a paged message uses it to find the
// incidents the message announced; a grouped message returns one row per
// incident.
func (s *Store) TelegramMessageDeliveries(ctx context.Context, chatID, messageID int64) ([]Delivery, error) {
	rows, err := s.pool.Query(ctx, `select `+deliveryColumns+`
		from deliveries d
		where d.telegram_chat_id = $1 and d.telegram_message_id = $2
		order by d.id`, chatID, messageID)
	if err != nil {
		return nil, fmt.Errorf("read telegram message deliveries: %w", err)
	}
	defer rows.Close()
	var deliveries []Delivery
	for rows.Next() {
		var delivery Delivery
		if err := scanDelivery(rows, &delivery); err != nil {
			return nil, fmt.Errorf("scan delivery: %w", err)
		}
		deliveries = append(deliveries, delivery)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read telegram message deliveries: %w", err)
	}
	return deliveries, nil
}

// AppDayStats sums the hourly data of every app over a time range.
func (s *Store) AppDayStats(ctx context.Context, from, to time.Time) ([]AppDayStats, error) {
	rows, err := s.pool.Query(ctx, `
		select a.id, a.name, sum(rh.requests), sum(rh.s5xx), raedar_hist_sum(rh.latency)
		from route_hour rh
		join routes r on r.id = rh.route_id
		join apps a on a.id = r.app_id
		where rh.ts >= $1 and rh.ts < $2
		group by a.id, a.name`, from, to)
	if err != nil {
		return nil, fmt.Errorf("read app day stats: %w", err)
	}
	defer rows.Close()
	var stats []AppDayStats
	for rows.Next() {
		var (
			entry     AppDayStats
			histogram []int32
		)
		if err := rows.Scan(&entry.AppID, &entry.Name, &entry.Requests, &entry.S5xx, &histogram); err != nil {
			return nil, fmt.Errorf("scan app day stats: %w", err)
		}
		if entry.Latency, err = LoadHistogram(histogram); err != nil {
			return nil, fmt.Errorf("read app day stats: %w", err)
		}
		stats = append(stats, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read app day stats: %w", err)
	}
	return stats, nil
}

// DockerDiskLatest returns the newest Docker disk usage report, or nil when
// there is none.
func (s *Store) DockerDiskLatest(ctx context.Context) (*DockerDisk, error) {
	var usage DockerDisk
	var images, imagesReclaimable, buildCache, volumes, containers int64
	err := s.pool.QueryRow(ctx, `
		select ts, images_bytes, images_reclaimable_bytes, build_cache_bytes, volumes_bytes, containers_bytes
		from docker_disk order by ts desc limit 1`).Scan(&usage.TS, &images, &imagesReclaimable, &buildCache,
		&volumes, &containers)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read Docker disk usage: %w", err)
	}
	usage.ImagesBytes, usage.ImagesReclaimableBytes = images, imagesReclaimable
	usage.BuildCacheBytes, usage.VolumesBytes, usage.ContainersBytes = buildCache, volumes, containers
	return &usage, nil
}

// DeliveryRow is one delivery joined with the incident it belongs to, for
// the deliveries page.
type DeliveryRow struct {
	Delivery
	Rule    string
	Subject string
}

// RecentDeliveries returns the newest deliveries across all incidents.
func (s *Store) RecentDeliveries(ctx context.Context, limit int) ([]DeliveryRow, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		select `+deliveryColumns+`, coalesce(i.rule, ''), coalesce(i.subject, '')
		from deliveries d
		left join incidents i on i.id = d.incident_id
		order by d.created_at desc, d.id desc
		limit $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("read recent deliveries: %w", err)
	}
	defer rows.Close()
	out := make([]DeliveryRow, 0, limit)
	for rows.Next() {
		var row DeliveryRow
		if err := scanDelivery(rows, &row.Delivery, &row.Rule, &row.Subject); err != nil {
			return nil, fmt.Errorf("scan delivery row: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read recent deliveries: %w", err)
	}
	return out, nil
}
