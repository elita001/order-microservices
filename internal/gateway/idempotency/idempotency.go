// Package idempotency makes retries of non-idempotent requests (POST) safe: requests with
// the same Idempotency-Key are executed once, and repeats get the stored response.
package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/elita001/order-microservices/pkg/auth"
	"github.com/elita001/order-microservices/pkg/breaker"
	"github.com/elita001/order-microservices/pkg/httpx"
	"github.com/elita001/order-microservices/pkg/logger"
)

// Config is usually loaded from environment variables with the IDEMPOTENCY_ prefix.
type Config struct {
	TTL     time.Duration `env:"TTL" envDefault:"24h"`      // how long a response is replayed
	LockTTL time.Duration `env:"LOCK_TTL" envDefault:"30s"` // longer than the upstream timeout
}

const (
	maxKeyLength = 255
	maxBodyBytes = 1 << 20

	stateProcessing = "processing"
	stateCompleted  = "completed"
)

// Headers of the stored response that are replayed; others (Date, X-Request-Id, rate
// limits) belong to the current request.
var replayedHeaders = []string{"Content-Type", "Location"}

type record struct {
	State       string            `json:"state"`
	Fingerprint string            `json:"fingerprint"` // hash of method, path, and body
	Status      int               `json:"status,omitempty"`
	Header      map[string]string `json:"header,omitempty"`
	Body        []byte            `json:"body,omitempty"`
}

type Store struct {
	rdb     *redis.Client
	cfg     Config
	timeout time.Duration // per Redis call
}

func New(rdb *redis.Client, cfg Config, timeout time.Duration) *Store {
	return &Store{rdb: rdb, cfg: cfg, timeout: timeout}
}

// Middleware handles requests with an Idempotency-Key header:
//
//   - the first request locks the key (SET NX), goes upstream, and its response is stored;
//   - a repeat while the first is in progress gets 409 and should be retried later;
//   - a repeat after it gets the stored response with Idempotent-Replayed: true;
//   - the same key with a different body gets 422.
//
// 5xx responses are not stored, so the client can retry. If Redis is unavailable, the
// request goes upstream with the key, and the order service deduplicates it in the
// database: Redis only answers repeats faster and keeps them away from the services.
func (s *Store) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get(httpx.IdempotencyKeyHeader)
		if key == "" {
			next.ServeHTTP(w, r)
			return
		}
		if len(key) > maxKeyLength {
			httpx.WriteError(w, http.StatusBadRequest, "invalid_idempotency_key", "Idempotency-Key must be at most 255 characters")
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			httpx.WriteError(w, http.StatusRequestEntityTooLarge, "body_too_large", "request body is too large")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		fingerprint := fingerprintOf(r, body)
		// Keys are chosen by each client independently, so they are scoped by user: another
		// user with the same key and body must not get this user's stored response.
		redisKey := "idempotency:" + key
		if p, ok := auth.FromContext(r.Context()); ok {
			redisKey = "idempotency:" + p.UserID.String() + ":" + key
		}
		log := logger.FromContext(r.Context())

		lock, _ := json.Marshal(record{State: stateProcessing, Fingerprint: fingerprint})
		ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
		acquired, err := s.rdb.SetNX(ctx, redisKey, lock, s.cfg.LockTTL).Result()
		cancel()
		if err != nil {
			requests.WithLabelValues(resultBypassed).Inc()
			warnUnavailable(log, err)
			next.ServeHTTP(w, r)
			return
		}
		if !acquired {
			s.handleRepeat(w, r, next, redisKey, fingerprint)
			return
		}

		requests.WithLabelValues(resultFirst).Inc()
		rec := newRecorder()
		next.ServeHTTP(rec, r)
		s.save(r.Context(), redisKey, fingerprint, rec)
		rec.writeTo(w)
	})
}

func (s *Store) handleRepeat(w http.ResponseWriter, r *http.Request, next http.Handler, redisKey, fingerprint string) {
	log := logger.FromContext(r.Context())
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	raw, err := s.rdb.Get(ctx, redisKey).Bytes()
	cancel()
	if err != nil {
		// The entry expired in between, or Redis failed: the upstream service deduplicates.
		requests.WithLabelValues(resultBypassed).Inc()
		if !errors.Is(err, redis.Nil) {
			warnUnavailable(log, err)
		}
		next.ServeHTTP(w, r)
		return
	}
	var rec record
	if err := json.Unmarshal(raw, &rec); err != nil {
		log.Error("corrupted idempotency record, relying on the upstream service", "error", err)
		next.ServeHTTP(w, r)
		return
	}

	switch {
	case rec.Fingerprint != fingerprint:
		requests.WithLabelValues(resultKeyReused).Inc()
		httpx.WriteError(w, http.StatusUnprocessableEntity, "idempotency_key_reused",
			"Idempotency-Key was already used for a different request")
	case rec.State == stateProcessing:
		requests.WithLabelValues(resultInProgress).Inc()
		w.Header().Set("Retry-After", "1")
		httpx.WriteError(w, http.StatusConflict, "request_in_progress",
			"a request with this Idempotency-Key is still in progress")
	default:
		requests.WithLabelValues(resultReplayed).Inc()
		for name, value := range rec.Header {
			w.Header().Set(name, value)
		}
		w.Header().Set(httpx.IdempotentReplayedHeader, "true")
		w.WriteHeader(rec.Status)
		_, _ = w.Write(rec.Body)
	}
}

// save stores a completed response or, for 5xx, releases the key so the client can retry.
// It must finish even if the client has disconnected, otherwise the key stays locked.
func (s *Store) save(ctx context.Context, redisKey, fingerprint string, rec *recorder) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.timeout)
	defer cancel()
	log := logger.FromContext(ctx)

	if rec.status >= http.StatusInternalServerError {
		if err := s.rdb.Del(ctx, redisKey).Err(); err != nil && !errors.Is(err, breaker.ErrOpen) {
			log.Warn("release idempotency key", "error", err) // the lock expires after LockTTL
		}
		return
	}

	header := make(map[string]string, len(replayedHeaders))
	for _, name := range replayedHeaders {
		if value := rec.header.Get(name); value != "" {
			header[name] = value
		}
	}
	data, err := json.Marshal(record{
		State:       stateCompleted,
		Fingerprint: fingerprint,
		Status:      rec.status,
		Header:      header,
		Body:        rec.body.Bytes(),
	})
	if err == nil {
		err = s.rdb.Set(ctx, redisKey, data, s.cfg.TTL).Err()
	}
	if err != nil && !errors.Is(err, breaker.ErrOpen) {
		// Not critical: repeats go upstream, where the database deduplicates them.
		log.Warn("save idempotent response", "error", err)
	}
}

// warnUnavailable logs a Redis failure; an open breaker is logged once by itself.
func warnUnavailable(log *slog.Logger, err error) {
	if !errors.Is(err, breaker.ErrOpen) {
		log.Warn("idempotency store unavailable, relying on the upstream service", "error", err)
	}
}

func fingerprintOf(r *http.Request, body []byte) string {
	h := sha256.New()
	h.Write([]byte(r.Method + " " + r.URL.Path + "\n"))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// recorder buffers the upstream response, so it can be stored before it is sent.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newRecorder() *recorder {
	return &recorder{header: http.Header{}, status: http.StatusOK}
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) WriteHeader(status int)      { r.status = status }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }

func (r *recorder) writeTo(w http.ResponseWriter) {
	for name, values := range r.header {
		w.Header()[name] = values
	}
	w.WriteHeader(r.status)
	_, _ = w.Write(r.body.Bytes())
}
