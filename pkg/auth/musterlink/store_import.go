package musterlink

import (
	"fmt"
	"log/slog"
)

// ImportBoltFile copies the links of the bolt store at path into dst and
// returns how many it added and how many the file holds. The file is opened
// read-only and left as it is, so a rollback to the bolt backend finds every
// link where it was. Links dst already holds are kept (see SecretStore.Import),
// which makes the import safe to run on every start while the file is around.
// Only counts are logged, never a record.
func ImportBoltFile(path string, key []byte, dst *SecretStore, logger *slog.Logger) (added, total int, err error) {
	if logger == nil {
		logger = slog.Default()
	}
	src, err := OpenBoltStoreReadOnly(path, key, logger)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = src.Close() }()

	links := map[string]*Link{}
	if err := src.Each(func(slackUserID string, link *Link) error {
		links[slackUserID] = link
		return nil
	}); err != nil {
		return 0, 0, fmt.Errorf("musterlink: read bolt %s: %w", path, err)
	}
	if len(links) == 0 {
		return 0, 0, nil
	}
	added, err = dst.Import(links)
	if err != nil {
		return 0, len(links), fmt.Errorf("musterlink: import into %s: %w", dst.Ref(), err)
	}
	return added, len(links), nil
}
