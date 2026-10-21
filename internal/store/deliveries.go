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
	// Status is pending, sent or failed.
	Status string
	// Error is the last error message; nil when there is none.
	Error *string
	// TelegramMessageID is the message_id Telegram returned from the
	// sendMessage call that delivered this row, when the channel is
	// telegram. The inbox replies use it to resolve reply-to messages.
	TelegramMessageID *int64
}

// AddDelivery records a new delivery and returns its ID.
func (s *Store) AddDelivery(ctx context.Context, delivery Delivery) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		insert into deliveries (incident_id, kind, channel, created_at, sent_at, attempts, status, error, telegram_message_id)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		returning id`,
		delivery.IncidentID, delivery.Kind, delivery.Channel, delivery.CreatedAt, delivery.SentAt,
		delivery.Attempts, delivery.Status, delivery.Error, delivery.TelegramMessageID).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert delivery: %w", err)
	}
	return id, nil
}

// UpdateDelivery records the outcome of a delivery attempt and, when the
// delivery succeeded via Telegram, the message_id Telegram returned. The
// caller learns the message_id from the Telegram API response and may pass
// it in the same call.
func (s *Store) UpdateDelivery(ctx context.Context, delivery Delivery) error {
	_, err := s.pool.Exec(ctx, `
		update deliveries set sent_at = $2, attempts = $3, status = $4, error = $5, telegram_message_id = $6 where id = $1`,
		delivery.ID, delivery.SentAt, delivery.Attempts, delivery.Status, delivery.Error, delivery.TelegramMessageID)
	if err != nil {
		return fmt.Errorf("update delivery %d: %w", delivery.ID, err)
	}
	return nil
}

// Deliveries returns the deliveries of one incident, newest first.
func (s *Store) Deliveries(ctx context.Context, incidentID int64) ([]Delivery, error) {
	rows, err := s.pool.Query(ctx, `
		select id, incident_id, kind, channel, created_at, sent_at, attempts, status, error, telegram_message_id
		from deliveries where incident_id = $1 order by created_at desc, id desc`, incidentID)
	if err != nil {
		return nil, fmt.Errorf("read deliveries: %w", err)
	}
	defer rows.Close()
	var deliveries []Delivery
	for rows.Next() {
		var delivery Delivery
		if err := rows.Scan(&delivery.ID, &delivery.IncidentID, &delivery.Kind, &delivery.Channel,
			&delivery.CreatedAt, &delivery.SentAt, &delivery.Attempts, &delivery.Status, &delivery.Error,
			&delivery.TelegramMessageID); err != nil {
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

// LookupDeliveryByTelegramMessageID returns the delivery whose
// telegram_message_id matches. Returns (nil, nil) when no row matches.
// Replies to a paged Telegram message use this to find the original
// (rule, subject) so that `mute 2h` knows what to silence.
func (s *Store) LookupDeliveryByTelegramMessageID(ctx context.Context, messageID int64) (*Delivery, error) {
	var delivery Delivery
	err := s.pool.QueryRow(ctx, `
		select id, incident_id, kind, channel, created_at, sent_at, attempts, status, error, telegram_message_id
		from deliveries where telegram_message_id = $1`, messageID).
		Scan(&delivery.ID, &delivery.IncidentID, &delivery.Kind, &delivery.Channel,
			&delivery.CreatedAt, &delivery.SentAt, &delivery.Attempts, &delivery.Status,
			&delivery.Error, &delivery.TelegramMessageID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read delivery by telegram message id: %w", err)
	}
	return &delivery, nil
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
