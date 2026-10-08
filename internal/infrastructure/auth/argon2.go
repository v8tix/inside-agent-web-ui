// Package auth is the driven adapter for password hashing.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/core/port/resource"
)

// Default cost: the OWASP-recommended argon2id setting (19 MiB, 2 iterations,
// 1 lane). It is encoded in every hash, so it can be raised later without
// invalidating existing passwords.
const (
	defaultMemoryKiB  = 19 * 1024
	defaultIterations = 2
	defaultThreads    = 1
	saltBytes         = 16
	keyBytes          = 32
)

// defaultMaxWait is how long a hash waits for a slot unless WithMaxWait says
// otherwise: long enough for a burst of sign-ins to be served in turn, short
// enough that a flood is turned away and not queued.
const defaultMaxWait = 5 * time.Second

// Sanity caps for parameters read back from a stored hash, so a corrupt or
// hostile value cannot make Verify allocate gigabytes or run for minutes.
const (
	maxMemoryKiB  = 256 * 1024
	maxIterations = 10
	maxThreads    = 16
)

var _ resource.PasswordHasher = (*Argon2)(nil)

// Argon2 hashes passwords with argon2id and the PHC string format:
//
//	$argon2id$v=19$m=19456,t=2,p=1$<salt>$<key>
type Argon2 struct {
	memory     uint32
	iterations uint32
	threads    uint8

	// slots limits how many hashes run at once: one takes a slot while it works.
	// Each holds its memory (19 MiB by default) for the whole of it, and signing
	// in is open to anyone, so without a limit a flood of attempts would hold
	// that many times the memory at once. The rest wait their turn.
	slots chan struct{}
	// maxWait is how long a hash waits for a slot before it gives up.
	maxWait time.Duration
	idKey   func(password, salt []byte, iterations, memory uint32, threads uint8, keyLen uint32) []byte
}

// Option customizes Argon2.
type Option func(*Argon2)

// WithParams sets the cost: memory in KiB, iterations and parallelism.
func WithParams(memoryKiB, iterations uint32, threads uint8) Option {
	return func(a *Argon2) { a.memory, a.iterations, a.threads = memoryKiB, iterations, threads }
}

// WithConcurrency sets how many hashes may run at once (at least one). The
// default is the number of processors: more would not be faster, only larger.
func WithConcurrency(n int) Option {
	return func(a *Argon2) { a.slots = make(chan struct{}, max(1, n)) }
}

// WithMaxWait sets how long a hash waits for its turn before it gives up with
// domain.ErrBusy. Zero or less means the default, five seconds.
func WithMaxWait(d time.Duration) Option {
	return func(a *Argon2) { a.maxWait = d }
}

// NewArgon2 returns a hasher with the default cost unless overridden.
func NewArgon2(opts ...Option) *Argon2 {
	a := &Argon2{memory: defaultMemoryKiB, iterations: defaultIterations, threads: defaultThreads, idKey: argon2.IDKey}
	for _, opt := range opts {
		opt(a)
	}
	if a.maxWait <= 0 {
		a.maxWait = defaultMaxWait
	}
	if a.slots == nil {
		WithConcurrency(runtime.GOMAXPROCS(0))(a)
	}
	return a
}

// derive is argon2id, once a slot is free. A free slot is taken at once. When
// none is, it waits until one is, ctx ends or maxWait has passed, and then gives
// up without running: with domain.ErrBusy after the wait, with ctx's error when
// the request is gone. A request that is gone already does not take a slot even
// when one is free.
func (a *Argon2) derive(ctx context.Context, password string, salt []byte, iterations, memory uint32, threads uint8, keyLen uint32) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, context.Cause(ctx)
	}
	select {
	case a.slots <- struct{}{}:
	default:
		waiting, stop := context.WithTimeoutCause(ctx, a.maxWait, domain.ErrBusy)
		defer stop()
		select {
		case a.slots <- struct{}{}:
		case <-waiting.Done():
			return nil, context.Cause(waiting)
		}
	}
	defer func() { <-a.slots }()
	return a.idKey([]byte(password), salt, iterations, memory, threads, keyLen), nil
}

var b64 = base64.RawStdEncoding

// Hash implements resource.PasswordHasher.
func (a *Argon2) Hash(ctx context.Context, password string) (string, error) {
	salt := make([]byte, saltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generating salt: %w", err)
	}
	key, err := a.derive(ctx, password, salt, a.iterations, a.memory, a.threads, keyBytes)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, a.memory, a.iterations, a.threads, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// Verify implements resource.PasswordHasher. It uses the parameters stored in
// hash and compares in constant time.
//
// Pattern: Bounded Verify
func (a *Argon2) Verify(ctx context.Context, password, hash string) (bool, error) {
	p, ok := parse(hash)
	if !ok {
		return false, nil
	}
	key, err := a.derive(ctx, password, p.salt, p.iterations, p.memory, p.threads, uint32(len(p.key)))
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(key, p.key) == 1, nil
}

type parsed struct {
	memory     uint32
	iterations uint32
	threads    uint8
	salt, key  []byte
}

func parse(hash string) (parsed, bool) {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v="+strconv.Itoa(argon2.Version) {
		return parsed{}, false
	}

	var p parsed
	params := strings.Split(parts[3], ",")
	if len(params) != 3 {
		return parsed{}, false
	}
	values := [3]uint64{}
	for i, name := range [3]string{"m=", "t=", "p="} {
		raw, found := strings.CutPrefix(params[i], name)
		if !found {
			return parsed{}, false
		}
		v, err := strconv.ParseUint(raw, 10, 32)
		if err != nil {
			return parsed{}, false
		}
		values[i] = v
	}
	if values[0] == 0 || values[0] > maxMemoryKiB ||
		values[1] == 0 || values[1] > maxIterations ||
		values[2] == 0 || values[2] > maxThreads {
		return parsed{}, false
	}
	p.memory, p.iterations, p.threads = uint32(values[0]), uint32(values[1]), uint8(values[2])

	var err error
	if p.salt, err = b64.DecodeString(parts[4]); err != nil || len(p.salt) == 0 {
		return parsed{}, false
	}
	if p.key, err = b64.DecodeString(parts[5]); err != nil || len(p.key) == 0 {
		return parsed{}, false
	}
	return p, true
}
