// session_pool.go
// Throwaway chat sessions and the async session pool.
//
// OpenAI-compatible clients are stateless: they re-send the entire
// conversation on every request. The bridge forwards that history to Z.AI
// inside a chat identified by chat_id, and every chat a completion references
// materialises server-side under the bridge account along with its history.
// Two problems follow if those chats are left behind:
//
//  1. Accumulation: one dead session per proxied request, forever.
//  2. Context rot: a chat that outlives its request means Z.AI's stored
//     history stacks on top of the history the client already re-sent, so the
//     model sees duplicated and stale context.
//
// So every request runs on a throwaway session that is deleted upstream as
// soon as its response is written or has definitively failed. Async mode (the
// default) keeps a standing batch of SESSION_POOL_SIZE ready sessions and
// refills it as each is consumed; --sync-mode mints one per request instead.
// Graceful shutdown drains in-flight requests, then deletes every session
// still sitting in the pool.
//
// Z.AI chat IDs are client-generated UUIDs, so minting is local and instant
// and an unconsumed session never touches the account. Deletion is one
// DELETE /api/v1/chats/{id} per chat, and the "could not find" reply counts as
// success, which keeps the collector idempotent.

package zbridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ErrPoolClosing is returned by Acquire once Shutdown has begun.
	ErrPoolClosing = errors.New("session pool is shutting down")
	// ErrPoolTimeout is returned by Acquire when no pooled session became
	// available within the configured wait window.
	ErrPoolTimeout = errors.New("timed out waiting for a pooled session")
)

const (
	// defaultPoolSize is the standing batch of pre-made ready sessions.
	defaultPoolSize = 5
	// defaultPoolWait bounds how long a completion request waits for a
	// pooled session before creating one directly (SESSION_ACQUIRE_TIMEOUT).
	defaultPoolWait = 10 * time.Second
	// poolOpTimeout bounds one upstream delete call.
	poolOpTimeout = 30 * time.Second
	// Retry delay when session creation fails. On Z.AI creation is local and
	// cannot fail, but a backend that calls upstream would need this.
	poolCreateBackoffStart = 1 * time.Second
	poolCreateBackoffMax   = 15 * time.Second
	// poolDrainWait bounds how long Shutdown waits for in-flight
	// retire/refill operations before reporting leftovers.
	poolDrainWait = 20 * time.Second
)

// SessionBackend is the slice of the Z.AI bridge the pool needs. Tests
// substitute a stub; the production backend is zaiSessionBackend.
type SessionBackend interface {
	CreateChatSession(ctx context.Context) (string, error)
	DeleteChatSession(ctx context.Context, sessionIDs ...string) error
}

// zaiSessionBackend implements SessionBackend against chat.z.ai.
type zaiSessionBackend struct{}

// NewZAIChatBackend returns the production SessionBackend.
func NewZAIChatBackend() SessionBackend { return zaiSessionBackend{} }

// CreateChatSession mints one fresh chat ID. Nothing happens upstream: a Z.AI
// chat only materialises when a completion first references it.
func (zaiSessionBackend) CreateChatSession(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return randomUUID(), nil
}

// DeleteChatSession deletes chats one by one (Z.AI has no bulk endpoint).
// Best-effort: every ID is attempted; the first error is returned after the
// rest have been tried.
func (zaiSessionBackend) DeleteChatSession(ctx context.Context, sessionIDs ...string) error {
	var firstErr error
	for _, id := range sessionIDs {
		if id == "" {
			continue
		}
		if err := DeleteZAIChat(ctx, id); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// DeleteZAIChat removes one chat from the Z.AI account. It is idempotent: an
// "already gone" reply counts as success, so the collector never gets stuck on
// a chat that was retired twice. A 401 forces a session re-init and one retry.
func DeleteZAIChat(ctx context.Context, chatID string) error {
	if chatID == "" {
		return nil
	}

	for attempt := 0; attempt < 2; attempt++ {
		session.mu.Lock()
		token := session.Token
		initialized := session.Initialized
		feVersion := session.FeVersion
		session.mu.Unlock()

		if token == "" || !initialized {
			if err := initializeSession(); err != nil {
				return fmt.Errorf("session init for chat delete: %s", err.Error())
			}
			continue // re-read the fresh token
		}

		urlStr := BASE_URL + "/api/v1/chats/" + chatID
		req, err := http.NewRequestWithContext(ctx, "DELETE", urlStr, nil)
		if err != nil {
			return fmt.Errorf("chat delete request build failed: %s", err.Error())
		}
		req.Header.Set("authorization", "Bearer "+token)
		req.Header.Set("User-Agent", zaiUserAgent)
		req.Header.Set("content-type", "application/json")
		req.Header.Set("x-fe-Version", feVersion)

		logDebugf("Z.AI chat delete: DELETE %s", urlStr)

		resp, err := zaiHTTPClient.Do(req)
		if err != nil {
			return fmt.Errorf("Z.AI chat delete connection error: %s", err.Error())
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		text := strings.TrimSpace(string(body))

		logDebugf("Z.AI chat delete response: %d %s", resp.StatusCode, text)

		switch {
		case resp.StatusCode == 401:
			// Token expired mid-flight: force re-init and retry once.
			session.mu.Lock()
			session.Initialized = false
			session.mu.Unlock()
			continue
		case resp.StatusCode == 404 || strings.Contains(text, "could not find"):
			return nil // already gone — nothing left to collect
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			return nil // "true" (or any 2xx) — deleted
		default:
			return fmt.Errorf("Z.AI chat delete failed: %d: %s", resp.StatusCode, text)
		}
	}
	return errors.New("chat delete: max retries exceeded")
}

// SessionPool holds the standing batch of ready chat sessions. Completions draw
// a pre-made session instead of minting one, and each consumed session is
// deleted upstream and replaced as soon as its response is processed.
type SessionPool struct {
	backend SessionBackend
	size    int

	ready chan string // buffered to size; members are unused, clean sessions

	stopOnce sync.Once
	stopCh   chan struct{}
	stopped  atomic.Bool

	wg sync.WaitGroup // outstanding create/delete operations
}

// NewSessionPool builds a pool that keeps size sessions ready, clamping size
// below 1 to the default. Call Start to begin warmup.
func NewSessionPool(backend SessionBackend, size int) *SessionPool {
	if size < 1 {
		size = defaultPoolSize
	}
	return &SessionPool{
		backend: backend,
		size:    size,
		ready:   make(chan string, size),
		stopCh:  make(chan struct{}),
	}
}

func (p *SessionPool) Size() int { return p.size }

// Ready reports how many sessions are currently stocked.
func (p *SessionPool) Ready() int { return len(p.ready) }

// Start pre-makes the initial batch.
func (p *SessionPool) Start() {
	logInfof("[Pool] warming up %d stateless session(s)...", p.size)
	for i := 0; i < p.size; i++ {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.fillSlot("warmup")
		}()
	}
}

// Acquire hands out one ready session, blocking until one is available, ctx is
// done, or wait elapses (wait <= 0 waits indefinitely). The caller must always
// call Release with the returned ID, including on error paths, or the session
// is never retired and the batch never refilled.
func (p *SessionPool) Acquire(ctx context.Context, wait time.Duration) (string, error) {
	var timeout <-chan time.Time
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		timeout = timer.C
	}
	for {
		select {
		case id := <-p.ready:
			logDebugf("[Pool] handed out session %s (%d/%d ready)", id, len(p.ready), p.size)
			return id, nil
		case <-ctx.Done():
			return "", ctx.Err()
		case <-timeout:
			return "", ErrPoolTimeout
		case <-p.stopCh:
			return "", ErrPoolClosing
		}
	}
}

// Release retires a consumed session: it is deleted upstream, then a
// replacement fills the gap in the batch. Both steps run in the background.
func (p *SessionPool) Release(sessionID string) {
	if sessionID == "" {
		return
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.deleteOne(sessionID, "used")
		if p.stopped.Load() {
			return // shutting down: retire only, don't rebuild the batch
		}
		p.fillSlot("refill")
	}()
}

// Shutdown stops refills, deletes every still-pooled session upstream, and
// waits (bounded) for in-flight retire/refill work. Sessions already checked
// out are deleted by their own request's Release.
func (p *SessionPool) Shutdown() {
	first := false
	p.stopOnce.Do(func() {
		first = true
		p.stopped.Store(true)
		close(p.stopCh)
	})
	if !first {
		return
	}

	var leftover []string
	for {
		select {
		case id := <-p.ready:
			leftover = append(leftover, id)
			continue
		default:
		}
		break
	}
	if len(leftover) > 0 {
		logConsolef("[Pool] clearing %d remaining session(s)...", len(leftover))
		ctx, cancel := context.WithTimeout(context.Background(), poolOpTimeout)
		err := p.backend.DeleteChatSession(ctx, leftover...)
		cancel()
		if err != nil {
			logWarnf("[Pool] failed to clear %d session(s): %v", len(leftover), err)
		} else {
			logInfof("[Pool] cleared %d pooled session(s): deleted %v", len(leftover), leftover)
		}
	} else {
		logInfof("[Pool] clearing all sessions... none remaining")
	}

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		logInfof("[Pool] all sessions accounted for")
	case <-time.After(poolDrainWait):
		logWarnf("[Pool] some background session operations did not finish within %s", poolDrainWait)
	}
}

// fillSlot creates one session, retrying through transient failures, and stocks
// it unless shutdown won the race. Synchronous; callers add the goroutine.
func (p *SessionPool) fillSlot(reason string) {
	backoff := poolCreateBackoffStart
	loggedOnce := false
	for {
		if p.stopped.Load() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), poolOpTimeout)
		id, err := p.backend.CreateChatSession(ctx)
		cancel()
		if err != nil {
			if p.stopped.Load() {
				return
			}
			// First failure is loud, repeats stay quiet so a misconfigured
			// backend cannot spam the log.
			if !loggedOnce {
				logErrorf("[Pool:%s] session creation failed (%v); retrying...", reason, err)
				loggedOnce = true
			} else {
				logDebugf("[Pool:%s] session creation failed again (%v); retrying in %s", reason, err, backoff)
			}
			select {
			case <-time.After(backoff):
			case <-p.stopCh:
				return
			}
			backoff *= 2
			if backoff > poolCreateBackoffMax {
				backoff = poolCreateBackoffMax
			}
			continue
		}
		p.stock(id, reason)
		return
	}
}

// stock adds a fresh session to the batch, or deletes it if shutdown raced in
// first, so the account never keeps sessions nobody will consume.
func (p *SessionPool) stock(id, reason string) {
	select {
	case p.ready <- id:
		logDebugf("[Pool:%s] session ready: %s (%d/%d)", reason, id, len(p.ready), p.size)
	case <-p.stopCh:
		p.deleteOne(id, "shutdown-race")
	}
}

// deleteOne deletes a single session upstream, best-effort.
func (p *SessionPool) deleteOne(id, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), poolOpTimeout)
	defer cancel()
	if err := p.backend.DeleteChatSession(ctx, id); err != nil {
		logWarnf("[Pool:%s] failed to delete session %s: %v", reason, id, err)
		return
	}
	logDebugf("[Pool:%s] deleted chat session: %s", reason, id)
}

// ============================================================================
// BRIDGE GLUE
// ============================================================================

var (
	// nil in sync mode, where the per-request flow still collects used
	// sessions.
	sessionPool *SessionPool
	// How long a request waits for a pooled session before creating one
	// directly. 0 waits forever. See SESSION_ACQUIRE_TIMEOUT.
	poolWait = defaultPoolWait
)

// AttachSessionPool swaps the pool and its acquire window, returning a function
// that restores the previous attachment. Passing nil switches to sync mode.
func AttachSessionPool(p *SessionPool, wait time.Duration) func() {
	oldPool, oldWait := sessionPool, poolWait
	sessionPool, poolWait = p, wait
	return func() {
		sessionPool, poolWait = oldPool, oldWait
	}
}

// AcquireStatelessSession returns a throwaway chat ID for one request. If a
// burst exhausts the batch the request waits up to poolWait, then creates a
// session directly rather than stalling indefinitely.
//
// The second return value reports whether the session is pool-owned (retired
// through pool.Release) or on-demand (retired through gcSessions).
func AcquireStatelessSession(ctx context.Context) (chatID string, pooled bool, err error) {
	if sessionPool == nil {
		return randomUUID(), false, nil
	}
	id, acqErr := sessionPool.Acquire(ctx, poolWait)
	switch {
	case acqErr == nil:
		logDebugf("[Pool] stateless session: %s (%d/%d ready)", id, sessionPool.Ready(), sessionPool.Size())
		return id, true, nil
	case errors.Is(acqErr, ErrPoolTimeout):
		chatID = randomUUID()
		logWarnf("[Pool] busy, session created on demand: %s", chatID)
		return chatID, false, nil
	default: // ErrPoolClosing, or the request's client went away
		if errors.Is(acqErr, ErrPoolClosing) {
			return "", false, errors.New("server is shutting down")
		}
		return "", false, acqErr
	}
}

// ReleaseStatelessSession retires a used chat session. Call it only once the
// response is fully written or has definitively failed: the chat is deleted on
// Z.AI, and in async mode the pool immediately stocks a replacement.
func ReleaseStatelessSession(chatID string, pooled bool) {
	if chatID == "" {
		return
	}
	if pooled && sessionPool != nil {
		sessionPool.Release(chatID)
		return
	}
	gcSessions("stateless", chatID)
}

// gcSessions deletes used-up chat sessions in the background, so response
// latency is unaffected. Best-effort: failures are logged and ignored.
func gcSessions(reason string, sessionIDs ...string) {
	ids := make([]string, 0, len(sessionIDs))
	for _, id := range sessionIDs {
		if id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return
	}
	go func() {
		// Its own context: the triggering request may already be gone.
		ctx, cancel := context.WithTimeout(context.Background(), poolOpTimeout)
		defer cancel()
		backend := zaiSessionBackend{}
		if err := backend.DeleteChatSession(ctx, ids...); err != nil {
			logWarnf("[GC:%s] failed to delete chat session(s) %v: %v", reason, ids, err)
			return
		}
		logDebugf("[GC:%s] deleted chat session(s): %v", reason, ids)
	}()
}

func sessionPoolStatus() map[string]interface{} {
	if sessionPool == nil {
		return map[string]interface{}{
			"mode":       "sync",
			"throwaway":  true,
			"gc_enabled": true,
		}
	}
	return map[string]interface{}{
		"mode":       "async",
		"throwaway":  true,
		"gc_enabled": true,
		"size":       sessionPool.Size(),
		"ready":      sessionPool.Ready(),
	}
}
