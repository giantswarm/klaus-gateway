package musterlink

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// DefaultSecretStoreName is the Secret the store uses when none is configured.
const DefaultSecretStoreName = "klaus-gateway-obo-links"

// SecretStore keeps every Link as one entry of a single Kubernetes Secret in
// the gateway's namespace, each value sealed with the store key exactly as
// BoltStore seals it on disk. It needs no volume, so a replacement pod comes up
// on any node the moment it is scheduled, and it is shared, so a second
// replica can read and write the same links.
//
// Writers use the Secret's resourceVersion for optimistic concurrency: a write
// re-reads the Secret and retries on a conflict, so concurrent writers (two
// replicas, or the import racing a sign-in) never clobber each other's
// entries. The Secret itself is created by the chart together with the Role
// that grants get/update/patch on it; the store does not create it.
type SecretStore struct {
	client    kubernetes.Interface
	namespace string
	name      string
	cipher    *linkCipher
	logger    *slog.Logger
	retries   int
	timeout   time.Duration

	// mu serializes this process's writers so they do not conflict with each
	// other; conflicts with other processes are handled by the retry.
	mu sync.Mutex
}

// SecretStoreOptions configure a SecretStore.
type SecretStoreOptions struct {
	// Namespace and Name locate the Secret. Name defaults to
	// DefaultSecretStoreName; Namespace is required.
	Namespace string
	Name      string
	// Retries bounds the resourceVersion conflict retries per write (default 5).
	Retries int
	// Timeout bounds every API call (default 10s).
	Timeout time.Duration
}

// NewSecretStore returns a SecretStore on client. key is resolved like the
// bolt store's key (32 raw bytes or a base64/hex encoding of them). The Secret
// is not touched here; Check verifies it is readable.
func NewSecretStore(client kubernetes.Interface, key []byte, opts SecretStoreOptions, logger *slog.Logger) (*SecretStore, error) {
	c, err := newLinkCipher(key)
	if err != nil {
		return nil, err
	}
	if opts.Namespace == "" {
		return nil, fmt.Errorf("musterlink: secret store needs a namespace")
	}
	if opts.Name == "" {
		opts.Name = DefaultSecretStoreName
	}
	if opts.Retries <= 0 {
		opts.Retries = 5
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &SecretStore{
		client:    client,
		namespace: opts.Namespace,
		name:      opts.Name,
		cipher:    c,
		logger:    logger,
		retries:   opts.Retries,
		timeout:   opts.Timeout,
	}, nil
}

// Ref is the namespace/name of the backing Secret, for logs.
func (s *SecretStore) Ref() string { return s.namespace + "/" + s.name }

// Check reads the Secret once and returns the number of links it holds. It
// fails when the Secret is missing or the ServiceAccount may not read it, so a
// misconfigured deployment fails at startup instead of treating every user as
// unlinked.
func (s *SecretStore) Check() (int, error) {
	ctx, cancel := s.context()
	defer cancel()
	sec, err := s.get(ctx)
	if err != nil {
		return 0, fmt.Errorf("musterlink: read secret %s: %w", s.Ref(), err)
	}
	return len(sec.Data), nil
}

// Get decrypts and returns the link for slackUserID, or (nil, false) when
// absent, and the API error when the Secret could not be read. A record that
// does not decode (written under another key, or corrupt) is logged and
// reported as ErrNotLinked: a re-link overwrites it, which heals it.
func (s *SecretStore) Get(slackUserID string) (*Link, error) {
	ctx, cancel := s.context()
	defer cancel()
	sec, err := s.get(ctx)
	if err != nil {
		return nil, fmt.Errorf("musterlink: read secret %s: %w", s.Ref(), err)
	}
	record, ok := sec.Data[slackUserID]
	if !ok {
		return nil, ErrNotLinked
	}
	link, err := s.cipher.openLink(record)
	if err != nil {
		s.logger.Error("musterlink: decode link failed, treating the user as unlinked", "err", err)
		return nil, ErrNotLinked
	}
	return link, nil
}

// Put encrypts and stores link. It returns the API error when the write did
// not go through (the Secret missing, the ServiceAccount not allowed to update
// it, the conflict retries exhausted, the apiserver away): the Secret then
// still holds the previous record.
func (s *SecretStore) Put(slackUserID string, link *Link) error {
	record, err := s.cipher.sealLink(link)
	if err != nil {
		return err
	}
	if err := s.mutate(func(data map[string][]byte) bool {
		data[slackUserID] = record
		return true
	}); err != nil {
		return fmt.Errorf("musterlink: write secret %s: %w", s.Ref(), err)
	}
	return nil
}

// Delete removes a link; missing keys are a no-op. It returns the API error
// when the write did not go through.
func (s *SecretStore) Delete(slackUserID string) error {
	if err := s.mutate(func(data map[string][]byte) bool {
		if _, ok := data[slackUserID]; !ok {
			return false
		}
		delete(data, slackUserID)
		return true
	}); err != nil {
		return fmt.Errorf("musterlink: delete from secret %s: %w", s.Ref(), err)
	}
	return nil
}

// Import adds every link in links that the Secret does not hold yet, in one
// write, and returns how many it added. An entry already present wins: it may
// carry a refresh token rotated after the source was last written, and
// overwriting it would burn that user's link.
func (s *SecretStore) Import(links map[string]*Link) (int, error) {
	records := make(map[string][]byte, len(links))
	for id, link := range links {
		record, err := s.cipher.sealLink(link)
		if err != nil {
			return 0, fmt.Errorf("musterlink: encrypt link: %w", err)
		}
		records[id] = record
	}
	added := 0
	err := s.mutate(func(data map[string][]byte) bool {
		added = 0
		for id, record := range records {
			if _, ok := data[id]; ok {
				continue
			}
			data[id] = record
			added++
		}
		return added > 0
	})
	if err != nil {
		return 0, err
	}
	return added, nil
}

func (s *SecretStore) context() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), s.timeout)
}

func (s *SecretStore) get(ctx context.Context) (*corev1.Secret, error) {
	return s.client.CoreV1().Secrets(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
}

// mutate applies fn to the Secret's current data and writes it back, retrying
// on a resourceVersion conflict. fn returns false when it changed nothing,
// which skips the write.
func (s *SecretStore) mutate(fn func(data map[string][]byte) bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := s.context()
	defer cancel()
	for i := 0; i < s.retries; i++ {
		sec, err := s.get(ctx)
		if err != nil {
			return err
		}
		if sec.Data == nil {
			sec.Data = map[string][]byte{}
		}
		if !fn(sec.Data) {
			return nil
		}
		_, err = s.client.CoreV1().Secrets(s.namespace).Update(ctx, sec, metav1.UpdateOptions{})
		if err == nil {
			return nil
		}
		if !apierrors.IsConflict(err) {
			return err
		}
	}
	return fmt.Errorf("secret %s: conflict retry limit exceeded", s.Ref())
}
