package middleware

import (
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
)

// RateLimiter implements a per-user in-memory sliding window rate limiter.
type RateLimiter struct {
	mu       sync.RWMutex
	windows  map[string]*window
	limits   map[string]int // tier -> requests per minute
	interval time.Duration
}

type window struct {
	timestamps []time.Time
	mu         sync.Mutex
}

// NewRateLimiter creates a new rate limiter with tier-based limits.
func NewRateLimiter(freeLimitPerMin, proLimitPerMin, enterpriseLimitPerMin int) *RateLimiter {
	rl := &RateLimiter{
		windows:  make(map[string]*window),
		limits:   make(map[string]int),
		interval: time.Minute,
	}
	rl.limits["free"] = freeLimitPerMin
	rl.limits["pro"] = proLimitPerMin
	if enterpriseLimitPerMin <= 0 {
		rl.limits["enterprise"] = 0 // unlimited
	} else {
		rl.limits["enterprise"] = enterpriseLimitPerMin
	}

	// Cleanup old entries periodically
	go rl.cleanup()
	return rl
}

// Middleware returns the Fiber rate limiting handler.
func (rl *RateLimiter) Middleware() fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID, _ := c.Locals("user_id").(string)
		tier, _ := c.Locals("tier").(string)

		if userID == "" || tier == "" {
			return c.Next()
		}

		limit, ok := rl.limits[tier]
		if !ok {
			limit = rl.limits["free"]
		}

		// Unlimited tier
		if limit == 0 {
			c.Set("X-RateLimit-Limit", "unlimited")
			return c.Next()
		}

		w := rl.getWindow(userID)
		now := time.Now()
		count := w.count(now, rl.interval)

		// Set rate limit headers
		remaining := limit - count - 1
		if remaining < 0 {
			remaining = 0
		}
		c.Set("X-RateLimit-Limit", itoa(limit))
		c.Set("X-RateLimit-Remaining", itoa(remaining))
		c.Set("X-RateLimit-Reset", itoa(int(now.Add(rl.interval).Unix())))

		if count >= limit {
			return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
				"error": fiber.Map{
					"code":    "RATE_LIMITED",
					"message": "Too many requests. Please try again later.",
					"status":  429,
				},
			})
		}

		w.add(now)
		return c.Next()
	}
}

func (rl *RateLimiter) getWindow(userID string) *window {
	rl.mu.RLock()
	w, ok := rl.windows[userID]
	rl.mu.RUnlock()
	if ok {
		return w
	}

	rl.mu.Lock()
	defer rl.mu.Unlock()
	// Double-check
	if w, ok := rl.windows[userID]; ok {
		return w
	}
	w = &window{}
	rl.windows[userID] = w
	return w
}

func (w *window) count(now time.Time, interval time.Duration) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	cutoff := now.Add(-interval)
	count := 0
	for _, t := range w.timestamps {
		if t.After(cutoff) {
			count++
		}
	}
	return count
}

func (w *window) add(t time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.timestamps = append(w.timestamps, t)
}

func (rl *RateLimiter) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		rl.mu.Lock()
		cutoff := time.Now().Add(-2 * rl.interval)
		for userID, w := range rl.windows {
			w.mu.Lock()
			fresh := w.timestamps[:0]
			for _, t := range w.timestamps {
				if t.After(cutoff) {
					fresh = append(fresh, t)
				}
			}
			w.timestamps = fresh
			if len(fresh) == 0 {
				delete(rl.windows, userID)
			}
			w.mu.Unlock()
		}
		rl.mu.Unlock()
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := make([]byte, 0, 10)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		buf = append(buf, byte('0'+n%10))
		n /= 10
	}
	if neg {
		buf = append(buf, '-')
	}
	// Reverse
	for i, j := 0, len(buf)-1; i < j; i, j = i+1, j-1 {
		buf[i], buf[j] = buf[j], buf[i]
	}
	return string(buf)
}
