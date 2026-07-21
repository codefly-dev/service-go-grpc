package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type syncTarget struct {
	relative  string
	directory bool
}

type syncTransaction struct {
	actualRoot      string
	stageRoot       string
	workspacePrefix string
	targets         map[string]syncTarget
}

func newSyncTransaction(actualRoot, workspacePrefix string) (*syncTransaction, error) {
	root, err := filepath.Abs(actualRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve service root: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("stat service root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("service root %q is not a directory", root)
	}
	stage, err := os.MkdirTemp("", "codefly-go-grpc-sync-*")
	if err != nil {
		return nil, fmt.Errorf("create sync stage: %w", err)
	}
	return &syncTransaction{
		actualRoot:      root,
		stageRoot:       stage,
		workspacePrefix: filepath.Clean(workspacePrefix),
		targets:         map[string]syncTarget{},
	}, nil
}

func (transaction *syncTransaction) StageRoot() string {
	return transaction.stageRoot
}

func (transaction *syncTransaction) Close() error {
	if transaction == nil || transaction.stageRoot == "" {
		return nil
	}
	return os.RemoveAll(transaction.stageRoot)
}

func (transaction *syncTransaction) CopyInput(relative string) error {
	relative, err := cleanSyncRelative(relative)
	if err != nil {
		return err
	}
	source := filepath.Join(transaction.actualRoot, relative)
	destination := filepath.Join(transaction.stageRoot, relative)
	if err := copySyncPath(source, destination); err != nil {
		return fmt.Errorf("stage sync input %q: %w", relative, err)
	}
	return nil
}

func (transaction *syncTransaction) TrackDirectory(relative string) error {
	return transaction.track(relative, true)
}

func (transaction *syncTransaction) TrackFile(relative string) error {
	return transaction.track(relative, false)
}

func (transaction *syncTransaction) track(relative string, directory bool) error {
	relative, err := cleanSyncRelative(relative)
	if err != nil {
		return err
	}
	for _, existing := range transaction.targets {
		if existing.relative == relative {
			if existing.directory != directory {
				return fmt.Errorf("sync target %q cannot be both a file and directory", relative)
			}
			return nil
		}
		if pathContains(existing.relative, relative) || pathContains(relative, existing.relative) {
			return fmt.Errorf("sync targets %q and %q overlap", existing.relative, relative)
		}
	}
	transaction.targets[relative] = syncTarget{relative: relative, directory: directory}
	return nil
}

func (transaction *syncTransaction) ChangedFiles() ([]string, error) {
	var changed []string
	for _, target := range transaction.sortedTargets() {
		targetChanges, err := transaction.targetChanges(target)
		if err != nil {
			return nil, err
		}
		for _, relative := range targetChanges {
			workspacePath := relative
			if transaction.workspacePrefix != "." && transaction.workspacePrefix != "" {
				workspacePath = filepath.Join(transaction.workspacePrefix, relative)
			}
			changed = append(changed, filepath.ToSlash(workspacePath))
		}
	}
	sort.Strings(changed)
	return compactStrings(changed), nil
}

const (
	syncIncomingSuffix = ".codefly-sync-incoming"
	syncBackupSuffix   = ".codefly-sync-backup"
)

// pendingSwap tracks one changed target through the two-phase apply.
type pendingSwap struct {
	relative string
	actual   string
	incoming string // staged replacement beside actual; "" when the target is being removed
	backup   string // displaced original beside actual; "" when no original existed
	swapped  bool
}

// Apply replaces every changed target with its staged content as one unit.
// Replacements are first fully materialized as suffixed siblings of their
// targets, so the copy work that can fail midway (I/O errors, disk full)
// happens before the real tree is touched. Each target then swaps in via
// renames within its parent directory, keeping the displaced original until
// every target has swapped; a failure mid-swap renames the originals back,
// so the tree is not left partially updated.
func (transaction *syncTransaction) Apply() error {
	swaps, err := transaction.prepareSwaps()
	if err != nil {
		discardIncoming(swaps)
		return err
	}
	for index, swap := range swaps {
		if err := swap.commit(); err != nil {
			err = fmt.Errorf("apply generated target %q: %w", swap.relative, err)
			if rollbackErr := rollbackSwaps(swaps[:index+1]); rollbackErr != nil {
				err = fmt.Errorf("%w (rollback also failed, displaced originals kept at %q siblings: %v)", err, syncBackupSuffix, rollbackErr)
			}
			discardIncoming(swaps)
			return err
		}
	}
	for _, swap := range swaps {
		if swap.backup != "" {
			_ = os.RemoveAll(swap.backup)
		}
	}
	return nil
}

func (transaction *syncTransaction) prepareSwaps() ([]*pendingSwap, error) {
	var swaps []*pendingSwap
	for _, target := range transaction.sortedTargets() {
		changed, err := transaction.targetChanges(target)
		if err != nil {
			return swaps, err
		}
		if len(changed) == 0 {
			continue
		}
		swap := &pendingSwap{
			relative: target.relative,
			actual:   filepath.Join(transaction.actualRoot, target.relative),
		}
		swaps = append(swaps, swap)
		staged := filepath.Join(transaction.stageRoot, target.relative)
		if _, err := os.Lstat(staged); os.IsNotExist(err) {
			continue // nothing staged: the swap removes the target
		} else if err != nil {
			return swaps, fmt.Errorf("stat staged target %q: %w", target.relative, err)
		}
		incoming := swap.actual + syncIncomingSuffix
		if err := os.RemoveAll(incoming); err != nil {
			return swaps, fmt.Errorf("prepare generated target %q: %w", target.relative, err)
		}
		if err := os.MkdirAll(filepath.Dir(swap.actual), 0o755); err != nil {
			return swaps, fmt.Errorf("prepare generated target %q: %w", target.relative, err)
		}
		swap.incoming = incoming
		if err := copySyncPath(staged, incoming); err != nil {
			return swaps, fmt.Errorf("prepare generated target %q: %w", target.relative, err)
		}
	}
	return swaps, nil
}

// commit moves the original aside and the replacement into place. Both steps
// are renames within the target's parent directory, so each is atomic.
func (swap *pendingSwap) commit() error {
	if _, err := os.Lstat(swap.actual); err == nil {
		backup := swap.actual + syncBackupSuffix
		if err := os.RemoveAll(backup); err != nil {
			return err
		}
		if err := os.Rename(swap.actual, backup); err != nil {
			return err
		}
		swap.backup = backup
	} else if !os.IsNotExist(err) {
		return err
	}
	if swap.incoming != "" {
		if err := os.Rename(swap.incoming, swap.actual); err != nil {
			return err
		}
	}
	swap.swapped = true
	return nil
}

func rollbackSwaps(swaps []*pendingSwap) error {
	var failures []error
	for index := len(swaps) - 1; index >= 0; index-- {
		swap := swaps[index]
		if swap.swapped && swap.incoming != "" {
			if err := os.RemoveAll(swap.actual); err != nil {
				failures = append(failures, err)
				continue
			}
		}
		if swap.backup != "" {
			if err := os.Rename(swap.backup, swap.actual); err != nil {
				failures = append(failures, err)
				continue
			}
			swap.backup = ""
		}
	}
	return errors.Join(failures...)
}

// discardIncoming removes replacement copies that never swapped in.
// Best-effort: leftovers are inert suffixed siblings, never live targets.
func discardIncoming(swaps []*pendingSwap) {
	for _, swap := range swaps {
		if swap.incoming != "" && !swap.swapped {
			_ = os.RemoveAll(swap.incoming)
		}
	}
}

func (transaction *syncTransaction) targetChanges(target syncTarget) ([]string, error) {
	actual := filepath.Join(transaction.actualRoot, target.relative)
	staged := filepath.Join(transaction.stageRoot, target.relative)
	if !target.directory {
		equal, err := syncNodesEqual(actual, staged)
		if err != nil {
			return nil, fmt.Errorf("compare generated file %q: %w", target.relative, err)
		}
		if equal {
			return nil, nil
		}
		return []string{target.relative}, nil
	}
	actualEntries, err := syncTreeEntries(actual)
	if err != nil {
		return nil, fmt.Errorf("inventory generated directory %q: %w", target.relative, err)
	}
	stagedEntries, err := syncTreeEntries(staged)
	if err != nil {
		return nil, fmt.Errorf("inventory staged directory %q: %w", target.relative, err)
	}
	keys := make(map[string]struct{}, len(actualEntries)+len(stagedEntries))
	for key := range actualEntries {
		keys[key] = struct{}{}
	}
	for key := range stagedEntries {
		keys[key] = struct{}{}
	}
	var changed []string
	for key := range keys {
		if !bytes.Equal(actualEntries[key], stagedEntries[key]) {
			changed = append(changed, filepath.Join(target.relative, key))
		}
	}
	sort.Strings(changed)
	return changed, nil
}

func (transaction *syncTransaction) sortedTargets() []syncTarget {
	targets := make([]syncTarget, 0, len(transaction.targets))
	for _, target := range transaction.targets {
		targets = append(targets, target)
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].relative < targets[j].relative })
	return targets
}

func syncTreeEntries(root string) (map[string][]byte, error) {
	entries := map[string][]byte{}
	if _, err := os.Lstat(root); os.IsNotExist(err) {
		return entries, nil
	} else if err != nil {
		return nil, err
	}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root || entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		digest, err := syncNodeDigest(path)
		if err != nil {
			return err
		}
		entries[relative] = digest
		return nil
	})
	return entries, err
}

func syncNodesEqual(left, right string) (bool, error) {
	leftDigest, leftErr := syncNodeDigest(left)
	rightDigest, rightErr := syncNodeDigest(right)
	if os.IsNotExist(leftErr) || os.IsNotExist(rightErr) {
		return os.IsNotExist(leftErr) && os.IsNotExist(rightErr), nil
	}
	if leftErr != nil {
		return false, leftErr
	}
	if rightErr != nil {
		return false, rightErr
	}
	return bytes.Equal(leftDigest, rightDigest), nil
}

func syncNodeDigest(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "%v:%04o:", info.Mode().Type(), info.Mode().Perm())
	switch {
	case info.Mode().IsRegular():
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if copyErr != nil {
			return nil, copyErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return nil, err
		}
		_, _ = io.WriteString(hash, target)
	default:
		return nil, fmt.Errorf("unsupported generated node mode %v", info.Mode())
	}
	return hash.Sum(nil), nil
}

func copySyncPath(source, destination string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	switch {
	case info.IsDir():
		if err := os.MkdirAll(destination, info.Mode().Perm()); err != nil {
			return err
		}
		entries, err := os.ReadDir(source)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := copySyncPath(filepath.Join(source, entry.Name()), filepath.Join(destination, entry.Name())); err != nil {
				return err
			}
		}
		return os.Chmod(destination, info.Mode().Perm())
	case info.Mode().IsRegular():
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return err
		}
		input, err := os.Open(source)
		if err != nil {
			return err
		}
		output, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			_ = input.Close()
			return err
		}
		_, copyErr := io.Copy(output, input)
		inputErr := input.Close()
		outputErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		if inputErr != nil {
			return inputErr
		}
		return outputErr
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(source)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return err
		}
		return os.Symlink(target, destination)
	default:
		return fmt.Errorf("unsupported sync input mode %v", info.Mode())
	}
}

func cleanSyncRelative(relative string) (string, error) {
	if strings.ContainsAny(relative, "\x00\\") || !filepath.IsLocal(relative) {
		return "", fmt.Errorf("sync path %q must stay within the service", relative)
	}
	relative = filepath.Clean(relative)
	if relative == "." {
		return "", fmt.Errorf("sync path must not own the service root")
	}
	return relative, nil
}

func pathContains(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func compactStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	result := values[:1]
	for _, value := range values[1:] {
		if value != result[len(result)-1] {
			result = append(result, value)
		}
	}
	return result
}
