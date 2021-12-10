package tuf

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/theupdateframework/go-tuf/data"
	"github.com/theupdateframework/go-tuf/encrypted"
	"github.com/theupdateframework/go-tuf/internal/sets"
	"github.com/theupdateframework/go-tuf/pkg/keys"
	"github.com/theupdateframework/go-tuf/util"
)

type LocalStore interface {
	// GetMeta returns a map from metadata file names (e.g. root.json) to their raw JSON payload or an error.
	GetMeta() (map[string]json.RawMessage, error)

	// SetMeta is used to update a metadata file name with a JSON payload.
	SetMeta(name string, meta json.RawMessage) error

	// WalkStagedTargets calls targetsFn for each staged target file in paths.
	// If paths is empty, all staged target files will be walked.
	WalkStagedTargets(paths []string, targetsFn TargetsWalkFunc) error

	// Commit is used to publish staged files to the repository
	Commit(consistentSnapshot bool, versions map[string]int, hashes map[string]data.Hashes) error

	// GetSigners return a list of signers for a role.
	GetSigners(role string) ([]keys.Signer, error)

	// SaveSigner adds a signer to a role.
	SaveSigner(role string, signer keys.Signer) error

	// SignersForRole return a list of signing keys for a role.
	SignersForKeyIDs(keyIDs []string) []keys.Signer

	// Clean is used to remove all staged manifests.
	Clean() error
}

type PassphraseChanger interface {
	// ChangePassphrase changes the passphrase for a role keys file.
	ChangePassphrase(string) error
}

func MemoryStore(meta map[string]json.RawMessage, files map[string][]byte) LocalStore {
	if meta == nil {
		meta = make(map[string]json.RawMessage)
	}
	return &memoryStore{
		meta:           meta,
		stagedMeta:     make(map[string]json.RawMessage),
		files:          files,
		signerForKeyID: make(map[string]keys.Signer),
		keyIDsForRole:  make(map[string][]string),
	}
}

type memoryStore struct {
	meta       map[string]json.RawMessage
	stagedMeta map[string]json.RawMessage
	files      map[string][]byte

	signerForKeyID map[string]keys.Signer
	keyIDsForRole  map[string][]string
}

func (m *memoryStore) GetMeta() (map[string]json.RawMessage, error) {
	meta := make(map[string]json.RawMessage, len(m.meta)+len(m.stagedMeta))
	for key, value := range m.meta {
		meta[key] = value
	}
	for key, value := range m.stagedMeta {
		meta[key] = value
	}
	return meta, nil
}

func (m *memoryStore) SetMeta(name string, meta json.RawMessage) error {
	m.stagedMeta[name] = meta
	return nil
}

func (m *memoryStore) WalkStagedTargets(paths []string, targetsFn TargetsWalkFunc) error {
	if len(paths) == 0 {
		for path, data := range m.files {
			if err := targetsFn(path, bytes.NewReader(data)); err != nil {
				return err
			}
		}
		return nil
	}

	for _, path := range paths {
		data, ok := m.files[path]
		if !ok {
			return ErrFileNotFound{path}
		}
		if err := targetsFn(path, bytes.NewReader(data)); err != nil {
			return err
		}
	}
	return nil
}

func (m *memoryStore) Commit(consistentSnapshot bool, versions map[string]int, hashes map[string]data.Hashes) error {
	for name, meta := range m.stagedMeta {
		paths := computeMetadataPaths(consistentSnapshot, name, versions)
		for _, path := range paths {
			m.meta[path] = meta
		}
	}
	return nil
}

func (m *memoryStore) GetSigners(role string) ([]keys.Signer, error) {
	keyIDs, ok := m.keyIDsForRole[role]
	if ok {
		return m.SignersForKeyIDs(keyIDs), nil
	}

	return nil, nil
}

func (m *memoryStore) SaveSigner(role string, signer keys.Signer) error {
	keyIDs := signer.PublicData().IDs()

	for _, keyID := range keyIDs {
		m.signerForKeyID[keyID] = signer
	}

	mergedKeyIDs := sets.DeduplicateStrings(append(m.keyIDsForRole[role], keyIDs...))
	m.keyIDsForRole[role] = mergedKeyIDs
	return nil
}

func (m *memoryStore) SignersForKeyIDs(keyIDs []string) []keys.Signer {
	signers := []keys.Signer{}
	keyIDsSeen := map[string]struct{}{}

	for _, keyID := range keyIDs {
		signer, ok := m.signerForKeyID[keyID]
		if !ok {
			continue
		}
		addSigner := false

		for _, skid := range signer.PublicData().IDs() {
			if _, seen := keyIDsSeen[skid]; !seen {
				addSigner = true
			}

			keyIDsSeen[skid] = struct{}{}
		}

		if addSigner {
			signers = append(signers, signer)
		}
	}

	return signers
}

func (m *memoryStore) Clean() error {
	return nil
}

type persistedKeys struct {
	Encrypted bool            `json:"encrypted"`
	Data      json.RawMessage `json:"data"`
}

func FileSystemStore(dir string, p util.PassphraseFunc) LocalStore {
	return &fileSystemStore{
		dir:            dir,
		passphraseFunc: p,
		signerForKeyID: make(map[string]keys.Signer),
		keyIDsForRole:  make(map[string][]string),
	}
}

type fileSystemStore struct {
	dir            string
	passphraseFunc util.PassphraseFunc

	signerForKeyID map[string]keys.Signer
	keyIDsForRole  map[string][]string
}

func (f *fileSystemStore) repoDir() string {
	return filepath.Join(f.dir, "repository")
}

func (f *fileSystemStore) stagedDir() string {
	return filepath.Join(f.dir, "staged")
}

func (f *fileSystemStore) GetMeta() (map[string]json.RawMessage, error) {
	meta := make(map[string]json.RawMessage)
	var err error
	notExists := func(path string) bool {
		_, err := os.Stat(path)
		return os.IsNotExist(err)
	}
	for _, name := range topLevelMetadata {
		path := filepath.Join(f.stagedDir(), name)
		if notExists(path) {
			path = filepath.Join(f.repoDir(), name)
			if notExists(path) {
				continue
			}
		}
		meta[name], err = ioutil.ReadFile(path)
		if err != nil {
			return nil, err
		}
	}
	return meta, nil
}

func (f *fileSystemStore) SetMeta(name string, meta json.RawMessage) error {
	if err := f.createDirs(); err != nil {
		return err
	}
	if err := util.AtomicallyWriteFile(filepath.Join(f.stagedDir(), name), meta, 0644); err != nil {
		return err
	}
	return nil
}

func (f *fileSystemStore) createDirs() error {
	for _, dir := range []string{"keys", "repository", "staged/targets"} {
		if err := os.MkdirAll(filepath.Join(f.dir, dir), 0755); err != nil {
			return err
		}
	}
	return nil
}

func (f *fileSystemStore) WalkStagedTargets(paths []string, targetsFn TargetsWalkFunc) error {
	if len(paths) == 0 {
		walkFunc := func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !info.Mode().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(filepath.Join(f.stagedDir(), "targets"), path)
			if err != nil {
				return err
			}
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			defer file.Close()
			return targetsFn(rel, file)
		}
		return filepath.Walk(filepath.Join(f.stagedDir(), "targets"), walkFunc)
	}

	// check all the files exist before processing any files
	for _, path := range paths {
		realPath := filepath.Join(f.stagedDir(), "targets", path)
		if _, err := os.Stat(realPath); err != nil {
			if os.IsNotExist(err) {
				return ErrFileNotFound{realPath}
			}
			return err
		}
	}

	for _, path := range paths {
		realPath := filepath.Join(f.stagedDir(), "targets", path)
		file, err := os.Open(realPath)
		if err != nil {
			if os.IsNotExist(err) {
				return ErrFileNotFound{realPath}
			}
			return err
		}
		err = targetsFn(path, file)
		file.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func (f *fileSystemStore) createRepoFile(path string) (*os.File, error) {
	dst := filepath.Join(f.repoDir(), path)
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return nil, err
	}
	return os.Create(dst)
}

func (f *fileSystemStore) Commit(consistentSnapshot bool, versions map[string]int, hashes map[string]data.Hashes) error {
	isTarget := func(path string) bool {
		return strings.HasPrefix(path, "targets/")
	}
	copyToRepo := func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(f.stagedDir(), path)
		if err != nil {
			return err
		}

		var paths []string
		if isTarget(rel) {
			paths = computeTargetPaths(consistentSnapshot, rel, hashes)
		} else {
			paths = computeMetadataPaths(consistentSnapshot, rel, versions)
		}
		var files []io.Writer
		for _, path := range paths {
			file, err := f.createRepoFile(path)
			if err != nil {
				return err
			}
			defer file.Close()
			files = append(files, file)
		}
		staged, err := os.Open(path)
		if err != nil {
			return err
		}
		defer staged.Close()
		if _, err = io.Copy(io.MultiWriter(files...), staged); err != nil {
			return err
		}
		return nil
	}
	needsRemoval := func(path string) bool {
		if consistentSnapshot {
			// strip out the hash
			name := strings.SplitN(filepath.Base(path), ".", 2)
			if len(name) != 2 || name[1] == "" {
				return false
			}
			path = filepath.Join(filepath.Dir(path), name[1])
		}
		_, ok := hashes[path]
		return !ok
	}
	removeFile := func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(f.repoDir(), path)
		if err != nil {
			return err
		}
		if !info.IsDir() && isTarget(rel) && needsRemoval(rel) {
			//lint:ignore SA9003 empty branch
			if err := os.Remove(path); err != nil {
				// TODO: log / handle error
			}
			// TODO: remove empty directory
		}
		return nil
	}
	if err := filepath.Walk(f.stagedDir(), copyToRepo); err != nil {
		return err
	}
	if err := filepath.Walk(f.repoDir(), removeFile); err != nil {
		return err
	}
	return f.Clean()
}

func (f *fileSystemStore) GetSigners(role string) ([]keys.Signer, error) {
	keyIDs, ok := f.keyIDsForRole[role]
	if ok {
		return f.SignersForKeyIDs(keyIDs), nil
	}

	privKeys, _, err := f.loadPrivateKeys(role)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	signers := []keys.Signer{}
	for _, key := range privKeys {
		signer, err := keys.GetSigner(key)
		if err != nil {
			return nil, err
		}

		// Cache the signers.
		for _, keyID := range signer.PublicData().IDs() {
			f.keyIDsForRole[role] = append(f.keyIDsForRole[role], keyID)
			f.signerForKeyID[keyID] = signer
		}
		signers = append(signers, signer)
	}

	return signers, nil
}

func (f *fileSystemStore) SignersForKeyIDs(keyIDs []string) []keys.Signer {
	signers := []keys.Signer{}
	keyIDsSeen := map[string]struct{}{}

	for _, keyID := range keyIDs {
		signer, ok := f.signerForKeyID[keyID]
		if !ok {
			continue
		}

		addSigner := false

		for _, skid := range signer.PublicData().IDs() {
			if _, seen := keyIDsSeen[skid]; !seen {
				addSigner = true
			}

			keyIDsSeen[skid] = struct{}{}
		}

		if addSigner {
			signers = append(signers, signer)
		}
	}

	return signers
}

// ChangePassphrase changes the passphrase for a role keys file. Implements
// PassphraseChanger interface.
func (f *fileSystemStore) ChangePassphrase(role string) error {
	// No need to proceed if passphrase func is not set
	if f.passphraseFunc == nil {
		return ErrPassphraseRequired{role}
	}
	// Read the existing keys (if any)
	// If encrypted, will prompt for existing passphrase
	keys, _, err := f.loadPrivateKeys(role)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Printf("Failed to change passphrase. Missing keys file for %s role. \n", role)
		}
		return err
	}
	// Prompt for new passphrase
	pass, err := f.passphraseFunc(role, true, true)
	if err != nil {
		return err
	}
	// Proceed saving the keys
	pk := &persistedKeys{Encrypted: true}
	pk.Data, err = encrypted.Marshal(keys, pass)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(pk, "", "\t")
	if err != nil {
		return err
	}
	if err := util.AtomicallyWriteFile(f.keysPath(role), append(data, '\n'), 0600); err != nil {
		return err
	}
	fmt.Printf("Successfully changed passphrase for %s keys file\n", role)
	return nil
}

func (f *fileSystemStore) SaveSigner(role string, signer keys.Signer) error {
	if err := f.createDirs(); err != nil {
		return err
	}

	// add the key to the existing keys (if any)
	privKeys, pass, err := f.loadPrivateKeys(role)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	key, err := signer.MarshalPrivateKey()
	if err != nil {
		return err
	}
	privKeys = append(privKeys, key)

	// if loadPrivateKeys didn't return a passphrase (because no keys yet exist)
	// and passphraseFunc is set, get the passphrase so the keys file can
	// be encrypted later (passphraseFunc being nil indicates the keys file
	// should not be encrypted)
	if pass == nil && f.passphraseFunc != nil {
		pass, err = f.passphraseFunc(role, true, false)
		if err != nil {
			return err
		}
	}

	pk := &persistedKeys{}
	if pass != nil {
		pk.Data, err = encrypted.Marshal(privKeys, pass)
		if err != nil {
			return err
		}
		pk.Encrypted = true
	} else {
		pk.Data, err = json.MarshalIndent(privKeys, "", "\t")
		if err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(pk, "", "\t")
	if err != nil {
		return err
	}
	if err := util.AtomicallyWriteFile(f.keysPath(role), append(data, '\n'), 0600); err != nil {
		return err
	}

	innerKeyIdsForRole := f.keyIDsForRole[role]

	for _, key := range privKeys {
		signer, err := keys.GetSigner(key)
		if err != nil {
			return err
		}

		keyIDs := signer.PublicData().IDs()

		for _, keyID := range keyIDs {
			f.signerForKeyID[keyID] = signer
		}

		innerKeyIdsForRole = append(innerKeyIdsForRole, keyIDs...)
	}

	f.keyIDsForRole[role] = sets.DeduplicateStrings(innerKeyIdsForRole)

	return nil
}

// loadPrivateKeys loads keys for the given role and returns them along with the
// passphrase (if read) so that callers don't need to re-read it.
func (f *fileSystemStore) loadPrivateKeys(role string) ([]*data.PrivateKey, []byte, error) {
	file, err := os.Open(f.keysPath(role))
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()

	pk := &persistedKeys{}
	if err := json.NewDecoder(file).Decode(pk); err != nil {
		return nil, nil, err
	}

	var keys []*data.PrivateKey
	if !pk.Encrypted {
		if err := json.Unmarshal(pk.Data, &keys); err != nil {
			return nil, nil, err
		}
		return keys, nil, nil
	}

	// the keys are encrypted so cannot be loaded if passphraseFunc is not set
	if f.passphraseFunc == nil {
		return nil, nil, ErrPassphraseRequired{role}
	}

	// try the empty string as the password first
	pass := []byte("")
	if err := encrypted.Unmarshal(pk.Data, &keys, pass); err != nil {
		pass, err = f.passphraseFunc(role, false, false)
		if err != nil {
			return nil, nil, err
		}
		if err = encrypted.Unmarshal(pk.Data, &keys, pass); err != nil {
			return nil, nil, err
		}
	}
	return keys, pass, nil
}

func (f *fileSystemStore) keysPath(role string) string {
	return filepath.Join(f.dir, "keys", role+".json")
}

func (f *fileSystemStore) Clean() error {
	_, err := os.Stat(filepath.Join(f.repoDir(), "root.json"))
	if os.IsNotExist(err) {
		return ErrNewRepository
	} else if err != nil {
		return err
	}
	if err := os.RemoveAll(f.stagedDir()); err != nil {
		return err
	}
	return os.MkdirAll(filepath.Join(f.stagedDir(), "targets"), 0755)
}

func computeTargetPaths(consistentSnapshot bool, name string, hashes map[string]data.Hashes) []string {
	if consistentSnapshot {
		return util.HashedPaths(name, hashes[name])
	} else {
		return []string{name}
	}
}

func computeMetadataPaths(consistentSnapshot bool, name string, versions map[string]int) []string {
	copyVersion := false

	switch name {
	case "root.json":
		copyVersion = true
	case "timestamp.json":
		copyVersion = false
	default:
		if consistentSnapshot {
			copyVersion = true
		} else {
			copyVersion = false
		}
	}

	paths := []string{name}
	if copyVersion {
		paths = append(paths, util.VersionedPath(name, versions[name]))
	}

	return paths
}
