// Package virtualfs provides a custom backend for rclone, designed to optimize file synchronization
// from remote sources by persisting file metadata and temporarily storing file content. This approach
// ensures that files are ingested from the remote only once, and files that have been processed locally
// are not re-synced, saving bandwidth and storage space.
//
// Key Features:
// - **Persist file metadata**: Even after files are deleted locally, their metadata remains, making it appear as if the files are still present to rclone.
// - **Temporary content storage**: File content is stored locally only as long as needed, freeing up space after processing.
// - **Optimize synchronization**: Only sync files that have changed on the remote. Processed files are not re-synced.
// - **SQLite database**: Stores metadata efficiently and allows for complex queries if needed.

package virtualfs

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/hash"
)

func init() {
	fs.Register(&fs.RegInfo{
		Name:        "virtualfs",
		Description: "Virtual Filesystem Backend",
		NewFs:       NewFs,
		Options: []fs.Option{{
			Name:     "root_directory",
			Help:     "Root directory where content and metadata are stored.",
			Default:  "./virtualfs_data",
			Advanced: false,
		}},
	})
}

// Options defines the configuration for this backend
type Options struct {
	RootDirectory string `config:"root_directory"`
}

// Fs represents the virtual filesystem
type Fs struct {
	name     string       // name of this remote
	root     string       // the path we are working on
	opt      Options      // options
	features *fs.Features // optional features
	db       *sql.DB      // SQLite database connection
	dbLock   sync.RWMutex // read-write lock for database operations
}

// Object represents a file object in the virtual filesystem
type Object struct {
	fs      *Fs
	remote  string
	size    int64
	modTime time.Time
	hasHash bool
	hash    string
	deleted bool
	isDir   bool
}

// NewFs constructs an Fs from the path, container:path
func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	fs.Infof(nil, "VirtualFS: Initializing new filesystem with name '%s' and root '%s'", name, root)

	// Parse config into Options struct
	opt := new(Options)
	err := configstruct.Set(m, opt)
	if err != nil {
		return nil, err
	}

	// Create root directory if it doesn't exist
	err = os.MkdirAll(opt.RootDirectory, 0755)
	if err != nil {
		return nil, fmt.Errorf("failed to create root directory: %w", err)
	}

	f := &Fs{
		name: name,
		root: root,
		opt:  *opt,
	}
	f.features = (&fs.Features{
		CanHaveEmptyDirectories: true,
	}).Fill(ctx, f)

	// Initialize SQLite database
	dbPath := path.Join(opt.RootDirectory, "virtualfs.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	f.db = db

	// Create tables if they don't exist
	err = f.createTables()
	if err != nil {
		return nil, fmt.Errorf("failed to create tables: %w", err)
	}

	fs.Infof(nil, "VirtualFS: Successfully initialized filesystem at '%s'", opt.RootDirectory)
	return f, nil
}

// createTables creates the necessary tables in the SQLite database
func (f *Fs) createTables() error {
	f.dbLock.Lock()
	defer f.dbLock.Unlock()

	_, err := f.db.Exec(`
		CREATE TABLE IF NOT EXISTS files (
			remote TEXT PRIMARY KEY,
			size INTEGER,
			mod_time DATETIME,
			has_hash BOOLEAN,
			hash TEXT,
			deleted BOOLEAN,
			is_dir BOOLEAN
		);
		CREATE INDEX IF NOT EXISTS idx_files_remote ON files(remote);
		CREATE INDEX IF NOT EXISTS idx_files_deleted ON files(deleted);
	`)
	return err
}

// ensureDirectoryStructure ensures that all parent directories of a given path exist in the database
func (f *Fs) ensureDirectoryStructure(remote string) error {
	f.dbLock.Lock()
	defer f.dbLock.Unlock()

	// Split the path into parts and ensure each directory exists
	parts := strings.Split(path.Dir(remote), "/")
	currentPath := ""
	tx, err := f.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	for _, part := range parts {
		if part == "" {
			continue
		}
		currentPath = path.Join(currentPath, part)
		query := `INSERT OR IGNORE INTO files (remote, size, mod_time, has_hash, hash, deleted, is_dir) VALUES (?, 0, ?, 0, '', 0, 1)`
		_, err := tx.Exec(query, currentPath, time.Now().Format(time.RFC3339))
		if err != nil {
			return fmt.Errorf("failed to insert directory %s: %w", currentPath, err)
		}
	}

	err = tx.Commit()
	if err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	return nil
}

// List the objects and directories in dir into entries
func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	fs.Infof(nil, "VirtualFS: Listing contents of directory: %s", dir)
	f.dbLock.RLock()
	defer f.dbLock.RUnlock()

	var query string
	var args []interface{}
	if dir == "" {
		query = `SELECT remote, size, mod_time, has_hash, hash, deleted, is_dir FROM files WHERE remote NOT LIKE '%/%' AND deleted = 0`
	} else {
		query = `SELECT remote, size, mod_time, has_hash, hash, deleted, is_dir FROM files WHERE remote LIKE ? AND deleted = 0`
		args = append(args, dir+"/%")
	}

	rows, err := f.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var o Object
		var modTime string
		err := rows.Scan(&o.remote, &o.size, &modTime, &o.hasHash, &o.hash, &o.deleted, &o.isDir)
		if err != nil {
			return nil, err
		}
		o.fs = f
		o.modTime, _ = time.Parse(time.RFC3339, modTime)
		if dir == "" || path.Dir(o.remote) == dir {
			if o.isDir {
				entries = append(entries, fs.NewDir(o.remote, o.modTime))
			} else {
				entries = append(entries, &o)
			}
		}
	}

	fs.Infof(nil, "VirtualFS: Listed %d entries in directory: %s", len(entries), dir)
	return entries, rows.Err()
}

// NewObject finds the Object at remote
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	f.dbLock.RLock()
	defer f.dbLock.RUnlock()

	query := `SELECT size, mod_time, has_hash, hash, deleted, is_dir FROM files WHERE remote = ?`
	var o Object
	var modTime string
	err := f.db.QueryRow(query, remote).Scan(&o.size, &modTime, &o.hasHash, &o.hash, &o.deleted, &o.isDir)
	if err == sql.ErrNoRows || o.deleted || o.isDir {
		fs.Infof(nil, "VirtualFS: Object not found for remote %s", remote)
		return nil, fs.ErrorObjectNotFound
	}
	if err != nil {
		fs.Errorf(nil, "VirtualFS: Error querying object for remote %s: %v", remote, err)
		return nil, err
	}
	o.fs = f
	o.remote = remote
	o.modTime, _ = time.Parse(time.RFC3339, modTime)
	fs.Infof(nil, "VirtualFS: Object found for remote %s", remote)
	return &o, nil
}

// Put the object
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	remote := src.Remote()
	fs.Infof(nil, "VirtualFS: Put called for remote %s", remote)

	existingObj, err := f.NewObject(ctx, remote)
	if err != nil && err != fs.ErrorObjectNotFound {
		return nil, err
	}

	shouldUpdate := true
	if err == nil {
		shouldUpdate = false
		if src.Size() != existingObj.Size() {
			shouldUpdate = true
		} else if !src.ModTime(ctx).Equal(existingObj.ModTime(ctx)) {
			shouldUpdate = true
		} else {
			srcSupportsMD5 := src.Fs().Hashes().Contains(hash.MD5) && f.Hashes().Contains(hash.MD5)
			if srcSupportsMD5 {
				hashSrc, _ := src.Hash(ctx, hash.MD5)
				if hashSrc != existingObj.(*Object).hash {
					shouldUpdate = true
				}
			}
		}

		if !shouldUpdate {
			fs.Infof(f, "Skipping identical file: %s", remote)
			return existingObj, nil
		}
	}

	filePath := f.fullPath(remote)

	fs.Infof(nil, "VirtualFS: Put called for remote %s", remote)

	// Ensure directory structure exists in the database
	err = f.ensureDirectoryStructure(remote)
	if err != nil {
		return nil, fmt.Errorf("failed to ensure directory structure: %w", err)
	}

	// Create parent directories in filesystem
	err = os.MkdirAll(path.Dir(filePath), 0755)
	if err != nil {
		return nil, err
	}

	outFile, err := os.Create(filePath)
	if err != nil {
		return nil, err
	}
	defer outFile.Close()

	// Compute hash while copying
	multiHasher, err := hash.NewMultiHasherTypes(hash.NewHashSet(hash.MD5))
	if err != nil {
		return nil, fmt.Errorf("failed to create multi hasher: %w", err)
	}
	teeReader := io.TeeReader(in, multiHasher)

	// Copy the content and compute hash
	size, err := io.Copy(outFile, teeReader)
	if err != nil {
		return nil, err
	}

	// Get the computed hash
	hashSum := multiHasher.Sums()[hash.MD5]
	hasHash := hashSum != ""

	// Create or update metadata in database
	f.dbLock.Lock()
	defer f.dbLock.Unlock()

	query := `INSERT OR REPLACE INTO files (remote, size, mod_time, has_hash, hash, deleted, is_dir) VALUES (?, ?, ?, ?, ?, ?, ?)`
	_, err = f.db.Exec(query, remote, size, src.ModTime(ctx).Format(time.RFC3339), hasHash, hashSum, false, false)
	if err != nil {
		return nil, err
	}

	// Return object
	return &Object{
		fs:      f,
		remote:  remote,
		size:    size,
		modTime: src.ModTime(ctx),
		hasHash: hasHash,
		hash:    hashSum,
		deleted: false,
		isDir:   false,
	}, nil
}

// Mkdir creates the container if it doesn't exist
func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	fs.Infof(nil, "VirtualFS: Mkdir called for directory %s", dir)
	dirPath := f.fullPath(dir)
	err := os.MkdirAll(dirPath, 0755)
	if err != nil {
		return err
	}

	return f.ensureDirectoryStructure(dir)
}

// Rmdir removes a directory if it's empty
func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	fs.Infof(nil, "VirtualFS: Rmdir called for directory %s", dir)
	dirPath := f.fullPath(dir)
	err := os.Remove(dirPath)
	if os.IsNotExist(err) {
		return fs.ErrorDirNotFound
	}
	if err != nil {
		return err
	}

	f.dbLock.Lock()
	defer f.dbLock.Unlock()

	// Check if the directory is empty in the database
	query := `SELECT COUNT(*) FROM files WHERE remote LIKE ? AND remote != ? AND deleted = 0`
	var count int
	err = f.db.QueryRow(query, dir+"/%", dir).Scan(&count)
	if err != nil {
		return err
	}

	if count > 0 {
		return fmt.Errorf("directory not empty")
	}

	// Remove the directory from the database
	query = `DELETE FROM files WHERE remote = ? AND is_dir = 1`
	_, err = f.db.Exec(query, dir)
	return err
}

// Name returns the name of the remote
func (f *Fs) Name() string {
	return f.name
}

// Root returns the root of the remote
func (f *Fs) Root() string {
	return f.root
}

// String converts this Fs to a string
func (f *Fs) String() string {
	return fmt.Sprintf("Virtual Filesystem at '%s'", f.opt.RootDirectory)
}

// Precision returns the precision of the remote
func (f *Fs) Precision() time.Duration {
	return time.Nanosecond
}

// Hashes returns the supported hash types
func (f *Fs) Hashes() hash.Set {
	return hash.Set(hash.MD5)
}

// Features returns the optional features of this Fs
func (f *Fs) Features() *fs.Features {
	return f.features
}

// fullPath returns the full path for a given remote path
func (f *Fs) fullPath(remote string) string {
	return path.Join(f.opt.RootDirectory, remote)
}

// ===== Object Methods =====

// Fs returns the parent Fs
func (o *Object) Fs() fs.Info {
	return o.fs
}

// Remote returns the remote path
func (o *Object) Remote() string {
	if o.deleted {
		return o.remote + ".delete"
	}
	return o.remote
}

// String returns a string representation of the object
func (o *Object) String() string {
	return o.Remote()
}

// ModTime returns the modification time of the object
func (o *Object) ModTime(ctx context.Context) time.Time {
	fs.Infof(nil, "VirtualFS: Getting mod time %v for remote %s", o.modTime, o.remote)
	return o.modTime
}

// Size returns the size of the object
func (o *Object) Size() int64 {
	size := o.size
	if o.deleted {
		size = 0
	}
	fs.Infof(nil, "VirtualFS: Getting size %d for remote %s", size, o.remote)
	return size
}

// Hash returns the MD5 hash of the object
func (o *Object) Hash(ctx context.Context, t hash.Type) (string, error) {
	if t != hash.MD5 {
		return "", hash.ErrUnsupported
	}
	if o.hasHash {
		fs.Infof(nil, "VirtualFS: Getting hash %v for remote %s", o.hash, o.remote)
		return o.hash, nil
	}
	fs.Infof(nil, "VirtualFS: No hash available for remote %s", o.remote)
	return "", nil
}

// Open opens the file for reading
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	return os.Open(o.fs.fullPath(o.remote))
}

// Remove removes the object
func (o *Object) Remove(ctx context.Context) error {
	fs.Infof(nil, "VirtualFS: Remove called for remote %s", o.remote)

	// Remove content
	err := os.Remove(o.fs.fullPath(o.remote))
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	// Create a .delete placeholder file to indicate deletion
	deletePath := o.fs.fullPath(o.remote + ".delete")
	deleteFile, err := os.Create(deletePath)
	if err != nil {
		return fmt.Errorf("failed to create delete placeholder: %w", err)
	}
	deleteFile.Close()

	// Update metadata in database
	o.fs.dbLock.Lock()
	defer o.fs.dbLock.Unlock()

	query := `UPDATE files SET deleted = 1, mod_time = ? WHERE remote = ?`
	_, err = o.fs.db.Exec(query, time.Now().Format(time.RFC3339), o.remote)
	if err != nil {
		return err
	}

	o.deleted = true

	return nil
}

// SetModTime sets the modification time of the object
func (o *Object) SetModTime(ctx context.Context, modTime time.Time) error {
	fs.Infof(nil, "VirtualFS: SetModTime called for remote %s", o.remote)
	o.fs.dbLock.Lock()
	defer o.fs.dbLock.Unlock()

	query := `UPDATE files SET mod_time = ? WHERE remote = ?`
	_, err := o.fs.db.Exec(query, modTime.Format(time.RFC3339), o.remote)
	if err != nil {
		return err
	}
	o.modTime = modTime
	return nil
}

// Update updates the object with new content
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	fs.Infof(nil, "VirtualFS: Update called for remote %s", o.remote)

	shouldUpdate := true
	if o.size == src.Size() {
		srcSupportsMD5 := src.Fs().Hashes().Contains(hash.MD5) && o.fs.Hashes().Contains(hash.MD5)
		hashh, _ := src.Hash(ctx, hash.MD5)
		if !srcSupportsMD5 || hashh == o.hash {
			if !src.ModTime(ctx).Equal(o.ModTime(ctx)) {
				shouldUpdate = false
			}
		}
	}

	if !shouldUpdate {
		fs.Infof(o.fs, "Skipping identical file: %s", o.remote)
		return nil
	}

	filePath := o.fs.fullPath(o.remote)
	err := os.MkdirAll(path.Dir(filePath), 0755)
	if err != nil {
		return err
	}

	outFile, err := os.Create(filePath)
	if err != nil {
		return err
	}
	defer outFile.Close()

	// Compute hash while copying
	multiHasher, err := hash.NewMultiHasherTypes(hash.NewHashSet(hash.MD5))
	if err != nil {
		return fmt.Errorf("failed to create multi hasher: %w", err)
	}
	teeReader := io.TeeReader(in, multiHasher)

	// Copy the content and compute hash
	size, err := io.Copy(outFile, teeReader)
	if err != nil {
		return err
	}

	// Get the computed hash
	hashSum := multiHasher.Sums()[hash.MD5]
	hasHash := hashSum != ""

	// Update metadata in database
	o.fs.dbLock.Lock()
	defer o.fs.dbLock.Unlock()

	query := `UPDATE files SET size = ?, mod_time = ?, has_hash = ?, hash = ?, deleted = 0, is_dir = 0 WHERE remote = ?`
	_, err = o.fs.db.Exec(query, size, src.ModTime(ctx).Format(time.RFC3339), hasHash, hashSum, o.remote)
	if err != nil {
		return err
	}

	o.size = size
	o.modTime = src.ModTime(ctx)
	o.hasHash = hasHash
	o.hash = hashSum
	o.deleted = false
	o.isDir = false

	return nil
}

// Storable indicates whether the object can be stored
func (o *Object) Storable() bool {
	return true
}

// Verify that all the interfaces are implemented correctly
var (
	_ fs.Fs       = (*Fs)(nil)
	_ fs.Object   = (*Object)(nil)
	_ fs.DirEntry = (*Object)(nil)
)
