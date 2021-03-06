package index

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/wpdirectory/wpdir/internal/codesearch/index"
	"github.com/wpdirectory/wpdir/internal/codesearch/regexp"
	"github.com/wpdirectory/wpdir/internal/filestats"
)

const (
	matchLimit               = 1000
	manifestFilename         = "metadata.gob"
	excludedFileJSONFilename = "excluded_files.json"
	filePeekSize             = 512
)

const (
	reasonDotFile     = "Dot files are excluded."
	reasonInvalidMode = "Invalid file mode."
	reasonNotText     = "Not a text file."
	reasonBinary      = "Binary files are excluded."
)

type Index struct {
	Ref *IndexRef
	idx *index.Index
	sync.RWMutex
}

type IndexOptions struct {
	ExcludeDotFiles bool
	SpecialFiles    []string
}

type SearchOptions struct {
	IgnoreCase     bool
	LinesOfContext uint
	FileRegexp     string
	IgnoreComments bool
	Offset         int
	Limit          int
}

type Match struct {
	Line       string
	LineNumber int
	Before     []string
	After      []string
}

type SearchResponse struct {
	Matches        []*FileMatch
	Slug           string
	FilesWithMatch int
	FilesOpened    int           `json:"-"`
	Duration       time.Duration `json:"-"`
	Revision       string
}

type FileMatch struct {
	Filename string
	Matches  []*Match
}

type ExcludedFile struct {
	Filename string
	Reason   string
}

type IndexRef struct {
	Time time.Time
	dir  string
	Slug string
}

func (r *IndexRef) Dir() string {
	return r.dir
}

func (r *IndexRef) writeManifest() error {
	w, err := os.Create(filepath.Join(r.dir, manifestFilename))
	if err != nil {
		return err
	}
	defer w.Close()

	return gob.NewEncoder(w).Encode(r)
}

func (r *IndexRef) Open() (*Index, error) {
	return &Index{
		Ref: r,
		idx: index.Open(filepath.Join(r.dir, "tri")),
	}, nil
}

func (r *IndexRef) Remove() error {
	return os.RemoveAll(r.dir)
}

func (n *Index) Close() error {
	n.Lock()
	defer n.Unlock()
	return n.idx.Close()
}

func (n *Index) Destroy() error {
	n.Lock()
	defer n.Unlock()
	if err := n.idx.Close(); err != nil {
		return err
	}
	return n.Ref.Remove()
}

// GetDir ...
func (n *Index) GetDir() string {
	return n.Ref.dir
}

func toStrings(lines [][]byte) []string {
	strs := make([]string, len(lines))
	for i, n := 0, len(lines); i < n; i++ {
		strs[i] = string(lines[i])
	}
	return strs
}

// GetRegexpPattern ...
func GetRegexpPattern(pat string, ignoreCase bool) string {
	if ignoreCase {
		return "(?i)(?m)" + pat
	}
	return "(?m)" + pat
}

// Search ...
func (n *Index) Search(pat, slug string, opt *SearchOptions) (*SearchResponse, error) {
	startedAt := time.Now()

	n.RLock()
	defer n.RUnlock()

	re, err := regexp.Compile(GetRegexpPattern(pat, opt.IgnoreCase))
	if err != nil {
		return nil, err
	}

	var (
		g                grepper
		results          []*FileMatch
		filesOpened      int
		filesFound       int
		filesCollected   int
		matchesCollected int
	)

	var fre *regexp.Regexp
	if opt.FileRegexp != "" {
		fre, err = regexp.Compile(opt.FileRegexp)
		if err != nil {
			return nil, err
		}
	}

	files := n.idx.PostingQuery(index.RegexpQuery(re.Syntax))
	for _, file := range files {
		var matches []*Match
		name := n.idx.Name(file)
		hasMatch := false

		// reject files that do not match the file pattern
		if fre != nil && fre.MatchString(name, true, true) < 0 {
			continue
		}

		filesOpened++
		if err := g.grep2File(filepath.Join(n.Ref.dir, "raw", name), re, int(opt.LinesOfContext),
			func(line []byte, lineno int, before [][]byte, after [][]byte) (bool, error) {

				hasMatch = true
				if filesFound < opt.Offset || (opt.Limit > 0 && filesCollected >= opt.Limit) {
					return false, nil
				}

				matchesCollected++
				matches = append(matches, &Match{
					Line:       string(line),
					LineNumber: lineno,
					Before:     toStrings(before),
					After:      toStrings(after),
				})

				if matchesCollected > matchLimit {
					return false, fmt.Errorf("search exceeds limit on matches: %d", matchLimit)
				}

				return true, nil
			}); err != nil {
			return nil, err
		}

		if !hasMatch {
			continue
		}

		filesFound++
		if len(matches) > 0 {
			filesCollected++
			results = append(results, &FileMatch{
				Filename: name,
				Matches:  matches,
			})
		}
	}

	return &SearchResponse{
		Matches:        results,
		FilesWithMatch: filesFound,
		FilesOpened:    filesOpened,
		Duration:       time.Now().Sub(startedAt),
	}, nil
}

func isBinaryFile(filename string) (bool, error) {
	buf := make([]byte, filePeekSize)
	r, err := os.Open(filename)
	if err != nil {
		return false, err
	}
	defer r.Close()

	n, err := io.ReadFull(r, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return false, err
	}

	buf = buf[:n]

	if detectBinary(buf) {
		return true, nil
	}

	return false, nil
}

// Adapted from The Platinum Searcher's detectEncoding() func.
// https://github.com/monochromegane/the_platinum_searcher/
func detectBinary(bs []byte) bool {
	var suspiciousBytes = 0

	length := len(bs)

	if length == 0 {
		return false
	}

	if length >= 3 && bs[0] == 0xEF && bs[1] == 0xBB && bs[2] == 0xBF {
		// UTF-8 BOM. This isn't binary.
		return false
	}

	if length >= 5 && bs[0] == 0x25 && bs[1] == 0x50 && bs[2] == 0x44 && bs[3] == 0x46 && bs[4] == 0x2D {
		/*  %PDF-. This is binary. */
		return true
	}

	for i := 0; i < length; i++ {
		if bs[i] == 0x00 {
			/* NULL char. It's binary */
			return true
		} else if (bs[i] < 7 || bs[i] > 14) && (bs[i] < 32 || bs[i] > 127) {
			suspiciousBytes++
			if i >= 32 && (suspiciousBytes*100)/length > 10 {
				return true
			}
		}
	}

	if (suspiciousBytes*100)/length > 10 {
		return true
	}

	return false
}

// Determines if the buffer contains valid UTF8 encoded string data. The buffer is assumed
// to be a prefix of a larger buffer so if the buffer ends with the start of a rune, it
// is still considered valid.
//
// Basic logic copied from https://golang.org/pkg/unicode/utf8/#Valid
func validUTF8IgnoringPartialTrailingRune(p []byte) bool {
	i := 0
	n := len(p)

	for i < n {
		if p[i] < utf8.RuneSelf {
			i++
		} else {
			_, size := utf8.DecodeRune(p[i:])
			if size == 1 {
				// All valid runes of size 1 (those below RuneSelf) were handled above. This must be a RuneError.
				// If we're encountering this error within UTFMax of the end and the current byte could be a
				// valid start, we'll just ignore the assumed partial rune.
				return n-i < utf8.UTFMax && utf8.RuneStart(p[i])
			}
			i += size
		}
	}
	return true
}

// write the list of excluded files to the given filename.
func writeExcludedFilesJSON(filename string, files []*ExcludedFile) error {
	w, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer w.Close()

	return json.NewEncoder(w).Encode(files)
}

func containsString(haystack []string, needle string) bool {
	for i, n := 0, len(haystack); i < n; i++ {
		if haystack[i] == needle {
			return true
		}
	}
	return false
}

// Read the metadata for the index directory. Note that even if this
// returns a non-nil error, a Metadata object will be returned with
// all the information that is known about the index (this might
// include only the path)
func Read(dir string) (*IndexRef, error) {
	m := &IndexRef{
		dir: dir,
	}

	r, err := os.Open(filepath.Join(dir, manifestFilename))
	if err != nil {
		return m, err
	}
	defer r.Close()

	if err := gob.NewDecoder(r).Decode(m); err != nil {
		return m, err
	}

	return m, nil
}

// Open the index in dir for searching.
func Open(dir string) (*Index, error) {
	r, err := Read(dir)
	if err != nil {
		return nil, err
	}

	return r.Open()
}

// BuildFromZip ...
func BuildFromZip(opt *IndexOptions, archive []byte, dst, slug string) (*IndexRef, *filestats.Stats, error) {

	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, nil, err
	}

	if err := os.Mkdir(dst, os.ModePerm); err != nil {
		return nil, nil, err
	}

	if err := os.Mkdir(filepath.Join(dst, "raw"), os.ModePerm); err != nil {
		return nil, nil, err
	}

	stats, err := indexAllZipFiles(opt, dst, zr.File)
	if err != nil {
		return nil, nil, err
	}

	r := &IndexRef{
		Time: time.Now(),
		dir:  dst,
		Slug: slug,
	}

	if err := r.writeManifest(); err != nil {
		return nil, nil, err
	}

	return r, stats, nil
}

func indexAllZipFiles(opt *IndexOptions, dst string, zfiles []*zip.File) (*filestats.Stats, error) {
	ix := index.Create(filepath.Join(dst, "tri"))
	defer ix.Close()

	excluded := []*ExcludedFile{}

	// Make a file to store the excluded files for this repo
	fileHandle, err := os.Create(filepath.Join(dst, "excluded_files.json"))
	if err != nil {
		return nil, err
	}
	defer fileHandle.Close()

	processFile := func(name string, file *zip.File) error {
		info := file.FileInfo()
		path := filepath.Dir(name)

		// Is this file considered "special", this means it's not even a part
		// of the source repository (like .git or .svn).
		if containsString(opt.SpecialFiles, name) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if opt.ExcludeDotFiles && name[0] == '.' {
			if info.IsDir() {
				return filepath.SkipDir
			}

			excluded = append(excluded, &ExcludedFile{
				name,
				reasonDotFile,
			})
			return nil
		}

		if info.IsDir() {
			return addZipDirToIndex(dst, name, path)
		}

		if info.Mode()&os.ModeType != 0 {
			excluded = append(excluded, &ExcludedFile{
				name,
				reasonInvalidMode,
			})
			return nil
		}

		txt, err := isZipTextFile(file)
		if err != nil {
			return err
		}

		if !txt {
			excluded = append(excluded, &ExcludedFile{
				name,
				reasonNotText,
			})
			return nil
		}

		reasonForExclusion, err := addZipFileToIndex(ix, dst, name, path, file)
		if err != nil {
			return err
		}
		if reasonForExclusion != "" {
			excluded = append(excluded, &ExcludedFile{name, reasonForExclusion})
		}

		return nil
	}

	stats := filestats.New()
	for _, file := range zfiles {
		if err = processFile(file.Name, file); err != nil {
			return nil, err
		}
		stats.AddFile(file)
	}
	stats.GenerateSummary()

	if err := writeExcludedFilesJSON(filepath.Join(dst, excludedFileJSONFilename), excluded); err != nil {
		return nil, err
	}

	ix.Flush()

	return stats, nil
}

func addZipFileToIndex(ix *index.IndexWriter, dst, src, path string, file *zip.File) (string, error) {
	r, err := file.Open()
	if err != nil {
		return "", err
	}
	defer r.Close()

	dup := filepath.Join(dst, "raw", file.Name)
	w, err := os.Create(dup)
	if err != nil {
		return "", err
	}
	defer w.Close()

	g := gzip.NewWriter(w)
	defer g.Close()

	return ix.Add(file.Name, io.TeeReader(r, g)), nil
}

func isZipTextFile(file *zip.File) (bool, error) {
	buf := make([]byte, filePeekSize)
	r, err := file.Open()
	if err != nil {
		return false, err
	}
	defer r.Close()

	n, err := io.ReadFull(r, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return false, err
	}

	buf = buf[:n]

	if n < filePeekSize {
		// read the whole file, must be valid.
		return utf8.Valid(buf), nil
	}

	// read a prefix, allow trailing partial runes.
	return validUTF8IgnoringPartialTrailingRune(buf), nil
}

func addZipDirToIndex(dst, src, path string) error {
	dup := filepath.Join(dst, "raw", path)
	return os.Mkdir(dup, 0766)
}
