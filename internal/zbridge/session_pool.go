// Throwaway chat sessions and the async session pool.
//
// OpenAI-compatible clients are stateless and re-send the whole conversation
// every time. Z.AI materialises a server-side chat with its own history for each
// chat_id a completion references, so leaving them behind causes two problems:
//
//  1. Accumulation: one dead session per proxied request, forever.
//  2. Context rot: Z.AI's stored history stacks on top of the history the client
//     already re-sent, so the model sees duplicated, stale context.
//
// So every request runs on a session deleted upstream as soon as its response is
// written or has definitively failed. Async mode (default) keeps a standing batch
// of SESSION_POOL_SIZE ready and refills as they are consumed; --sync-mode mints
// one per request. Shutdown drains in-flight requests, then deletes the rest.
//
// Chat IDs are client-generated UUIDs, so minting is local and instant and an
// unconsumed session never touches the account. Deleting is one DELETE per chat,
// and a "could not find" reply counts as success so the operation is idempotent.

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
	// ErrPoolClosing comes back from Acquire once Shutdown has begun.
	ErrPoolClosing = errors.New("session pool is shutting down")
	// ErrPoolTimeout means no session freed up inside the wait window.
	ErrPoolTimeout = errors.New("timed out waiting for a pooled session")
)

const (
	// The standing batch of pre-made ready sessions.
	defaultPoolSize = 5
	// How long a request waits before minting its own (SESSION_ACQUIRE_TIMEOUT).
	defaultPoolWait = 10 * time.Second
	// Bounds one upstream delete call.
	poolOpTimeout = 30 * time.Second
	// Creation cannot fail on Z.AI (it is local), but a backend that called
	// upstream would need this retry delay.
	poolCreateBackoffStart = 1 * time.Second
	poolCreateBackoffMax   = 15 * time.Second
	// How long Shutdown waits for in-flight work before reporting leftovers.
	poolDrainWait = 20 * time.Second
)

// SessionBackend is the slice of the bridge the pool needs; tests substitute a
// stub for zaiSessionBackend.
type SessionBackend interface {
	CreateChatSession(ctx context.Context) (string, error)
	DeleteChatSession(ctx context.Context, sessionIDs ...string) error
}

// zaiSessionBackend is SessionBackend against chat.z.ai.
type zaiSessionBackend struct{}

// NewZAIChatBackend returns the production backend.
func NewZAIChatBackend() SessionBackend { return zaiSessionBackend{} }

// CreateChatSession mints a chat ID locally; the chat only materialises upstream
// when a completion first references it.
func (zaiSessionBackend) CreateChatSession(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return randomUUID(), nil
}

// DeleteChatSession deletes one at a time, since Z.AI has no bulk endpoint. Every
// ID is attempted; the first error is returned once the rest have been tried.
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

// DeleteZAIChat removes one chat, idempotently: an "already gone" reply counts as
// success so a double retire cannot wedge the collector. A 401 forces one re-init
// and retry.
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
			// Token expired mid-flight: re-init and retry once.
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

// SessionPool holds the standing batch. Completions draw a pre-made session
// rather than minting one, and each consumed session is deleted upstream and
// replaced once its response is processed.
type SessionPool struct {
	backend SessionBackend
	size    int

	ready chan string // buffered to size; members are unused, clean sessions

	stopOnce sync.Once
	stopCh   chan struct{}
	stopped  atomic.Bool

	wg sync.WaitGroup // outstanding create/delete operations
}

// NewSessionPool keeps size sessions ready, clamping below 1 to the default.
// Call Start to warm up.
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

// Ready reports how many sessions are stocked right now.
func (p *SessionPool) Ready() int { return len(p.ready) }

// Start warms the initial batch in the background.
func (p *SessionPool) Start() {
	logInfof("[Pool] warming up %d stateless session(s)...", p.size)
	for i := 0; i < p.size; i++ {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			defer recoverGoroutine("session pool warmup")
			p.fillSlot("warmup")
		}()
	}
}

// Acquire hands out one ready session, blocking until one frees up, ctx is done,
// or wait elapses (wait <= 0 waits forever). The caller must always Release the
// returned ID, error paths included, or the batch is never refilled.
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

// Release deletes the session upstream, then refills the gap. Both in background.
func (p *SessionPool) Release(sessionID string) {
	if sessionID == "" {
		return
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer recoverGoroutine("session pool release")
		p.deleteOne(sessionID, "used")
		if p.stopped.Load() {
			return // shutting down: retire only, don't rebuild the batch
		}
		p.fillSlot("refill")
	}()
}

// Shutdown stops refills, deletes every still-pooled session and waits, bounded,
// for in-flight retire and refill work. Checked-out sessions are handled by their
// own request's Release.
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

// fillSlot creates one session, retrying transient failures, and stocks it unless
// shutdown won the race. Synchronous; the caller supplies the goroutine.
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
			// Loud once, then quiet: a misconfigured backend must not spam.
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

// stock adds a session to the batch, or deletes it if shutdown raced in, so the
// account never holds sessions nobody will consume.
func (p *SessionPool) stock(id, reason string) {
	select {
	case p.ready <- id:
		logDebugf("[Pool:%s] session ready: %s (%d/%d)", reason, id, len(p.ready), p.size)
	case <-p.stopCh:
		p.deleteOne(id, "shutdown-race")
	}
}

// deleteOne retires one session upstream, best-effort.
func (p *SessionPool) deleteOne(id, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), poolOpTimeout)
	defer cancel()
	if err := p.backend.DeleteChatSession(ctx, id); err != nil {
		logWarnf("[Pool:%s] failed to delete session %s: %v", reason, id, err)
		return
	}
	logDebugf("[Pool:%s] deleted chat session: %s", reason, id)
}

// Glue between the pool and the request path: the handlers acquire and release
// through these rather than touching the pool directly.

var (
	// nil in sync mode, where the per-request flow still collects used sessions.
	sessionPool *SessionPool
	// 0 waits forever. See SESSION_ACQUIRE_TIMEOUT.
	poolWait = defaultPoolWait
)

// AttachSessionPool swaps the pool and its wait window, returning a restore func.
// nil switches to sync mode.
func AttachSessionPool(p *SessionPool, wait time.Duration) func() {
	oldPool, oldWait := sessionPool, poolWait
	sessionPool, poolWait = p, wait
	return func() {
		sessionPool, poolWait = oldPool, oldWait
	}
}

// AcquireStatelessSession returns a throwaway chat ID. A burst that exhausts the
// batch waits up to poolWait, then mints one directly rather than stalling. The
// bool reports pool-owned (retired via pool.Release) versus on-demand (gcSessions).
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

// ReleaseStatelessSession retires a used session. Call it only once the response
// is fully written or has definitively failed: the chat is deleted upstream and,
// in async mode, immediately replaced.
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

// sessionGCWait tracks background deletes so shutdown can wait for them.
var sessionGCWait sync.WaitGroup

const sessionGCDrain = 8 * time.Second

// waitForSessionGC waits for in-flight deletes, bounded so a hung upstream cannot
// block exit.
func waitForSessionGC(limit time.Duration) {
	done := make(chan struct{})
	go func() {
		sessionGCWait.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(limit):
		logWarnf("[Shutdown] gave up waiting for session cleanup after %s", limit)
	}
}

// gcSessions deletes spent sessions in the background so response latency is
// unaffected. Failures are logged and ignored.
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
	sessionGCWait.Add(1)
	go func() {
		defer sessionGCWait.Done()
		defer recoverGoroutine("session GC")
		// Its own context, because the triggering request may already be gone.
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
