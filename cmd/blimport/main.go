// Command blimport is the development-only, one-way converter from the log-text
// store (log.jsonl) to this branch's SQLite store (r83 plan S1.5). It exists so the
// two branches can be measured on the same corpus: every entry keeps its id and its
// timestamp, and seq is rebuilt in order (ids are ids). It is not a migration path
// for users and ships in no daemon.
//
// The fold it applies is the log's own, reduced to what the tree needs: a patch
// replaces what an entry was (its replaced form goes to entry_rev — stamped with an
// import seq, since the log never kept patch seqs anywhere readable and it is the
// revision chain that matters, not its stamps), a delete removes an entry and
// re-attaches its children, and the last tip stated wins. Images cross into the
// blob table through the very AppendEntry/Patch path the daemon uses, which makes
// every import a rehearsal of the store's blob split (S3 by the way).
//
// Usage:
//
//	blimport [-force] [-verify] [-compress gzip|zstd|none] [-zdict file] <src-data-dir> <dst-data-dir> [session ...]
//	blimport -gendict [-dictout zdict.bin] [-maxdict 32768] <store-data-dir>
//
// -gendict is the other mode: it trains a zstd dictionary from the payloads of every
// store under the given directory (a daemon data dir, or one session dir) and writes
// it out — a release act for the seed book (embed the result as internal/store/
// seeddict.bin) or the user act for a personal one (point -zdict at it). Dictionaries
// are trained for the store's write level, so this tool and the daemon cannot drift.
//
// A session directory is imported when it holds log.jsonl and config.json; the
// config is copied verbatim. An existing dst store.db is refused unless -force,
// which drops the store (and only the store). -verify re-opens every imported store
// and prints its aggregates and a payload digest — two runs over one corpus agree
// exactly when the digests do.
package main

import (
	"bufio"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/klauspost/compress/dict"
	"github.com/klauspost/compress/zstd"

	_ "github.com/ncruces/go-sqlite3/driver"

	"bareloop/internal/ir"
	"bareloop/internal/store"
)

// record mirrors one line of log.jsonl — the schema the log-text branch writes.
type record struct {
	Op    string    `json:"op"`
	Seq   int64     `json:"seq"`
	Entry *ir.Entry `json:"entry,omitempty"`
	Tip   string    `json:"tip,omitempty"`
	ID    string    `json:"id,omitempty"`
}

// idShape is the only id the log ever handed out; anything else is a hand-edited
// file, which the plan says to refuse rather than guess about.
var idShape = regexp.MustCompile(`^[0-9]{7}$`)

func main() {
	force := flag.Bool("force", false, "replace an existing destination store")
	verify := flag.Bool("verify", false, "re-open imported stores and print aggregates + payload digest")
	comp := flag.String("compress", "zstd", `compress new payloads: zstd | gzip | none (the -verify digest weighs the text either way)`)
	zdictPath := flag.String("zdict", "", "zstd dictionary file: new zstd writes are stamped with it (keep the file: its frames need it to read back)")
	gendict := flag.Bool("gendict", false, "train a zstd dictionary from the store payloads under <src-data-dir>, write -dictout, and exit")
	dictout := flag.String("dictout", "zdict.bin", "output file for -gendict")
	maxdict := flag.Int("maxdict", 32768, "dictionary budget for -gendict, bytes")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: blimport [-force] [-verify] [-compress zstd|gzip|none] [-zdict file] <src-data-dir> <dst-data-dir> [session ...]")
		fmt.Fprintln(os.Stderr, "       blimport -gendict [-dictout zdict.bin] [-maxdict 32768] <store-data-dir>")
		flag.PrintDefaults()
	}
	flag.Parse()
	switch *comp {
	case "gzip", "zstd", "none":
		store.Compressor = *comp
	default:
		fail("compress: %q is not gzip, zstd or none", *comp)
	}
	if *zdictPath != "" {
		b, err := os.ReadFile(*zdictPath)
		if err != nil {
			fail("zdict: %v", err)
		}
		if err := store.SetZdict(b); err != nil {
			fail("%v", err)
		}
		if info, err := zstd.InspectDictionary(b); err == nil {
			fmt.Printf("zstd dictionary: %d bytes id=0x%08x\n", len(b), info.ID())
		}
	}
	args := flag.Args()
	if *gendict {
		if len(args) < 1 {
			flag.Usage()
			os.Exit(2)
		}
		genDictionary(args[0], *dictout, *maxdict)
		return
	}
	if len(args) < 2 {
		flag.Usage()
		os.Exit(2)
	}
	srcRoot, dstRoot := args[0], args[1]
	sessions := args[2:]

	dirs := sessions
	if len(dirs) == 0 {
		entries, err := os.ReadDir(srcRoot)
		if err != nil {
			fail("read %s: %v", srcRoot, err)
		}
		for _, de := range entries {
			if de.IsDir() {
				dirs = append(dirs, de.Name())
			}
		}
		if len(dirs) == 0 {
			fail("no session directories under %s", srcRoot)
		}
	}

	bad := 0
	t0 := time.Now()
	for _, id := range dirs {
		start := time.Now()
		n, err := importSession(srcRoot, dstRoot, id, *force, *verify)
		el := time.Since(start).Round(time.Millisecond)
		if err != nil {
			fmt.Fprintf(os.Stderr, "import %s: %v\n", id, err)
			bad++
			continue
		}
		if n >= 0 {
			fmt.Printf("imported %s: %d records replayed in %v\n", id, n, el)
		}
	}
	fmt.Printf("imported %d session(s) in %v (compress=%s)\n", len(dirs)-bad, time.Since(t0).Round(time.Millisecond), store.Compressor)
	if bad > 0 {
		fmt.Fprintf(os.Stderr, "%d session(s) failed\n", bad)
		os.Exit(1)
	}
}

// importSession replays one session's log into a fresh store. It returns the number
// of records applied, or -1 when the session was skipped (nothing to import).
func importSession(srcRoot, dstRoot, id string, force, verify bool) (int, error) {
	srcDir := filepath.Join(srcRoot, id)
	if _, err := os.Stat(filepath.Join(srcDir, "log.jsonl")); err != nil {
		return -1, nil // not a session log directory; silence is honest here
	}
	cfg, cfgErr := os.Stat(filepath.Join(srcDir, "config.json"))
	if cfgErr != nil {
		fmt.Fprintf(os.Stderr, "skip %s: no config.json (the daemon would not list it anyway)\n", id)
		return -1, nil
	}
	dstDir := filepath.Join(dstRoot, id)
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return -1, err
	}
	dst := filepath.Join(dstDir, store.DBFileName)
	if _, err := os.Stat(dst); err == nil {
		if !force {
			return -1, fmt.Errorf("%s exists; -force replaces it", dst)
		}
		for _, suffix := range []string{"", "-wal", "-shm"} {
			os.Remove(dst + suffix)
		}
	}
	// config.json is the session's other half; the copy is verbatim and unconditional,
	// so a -force re-import cannot leave a store newer than its config by accident.
	if err := copyFile(filepath.Join(srcDir, "config.json"), filepath.Join(dstDir, "config.json"), cfg.Size()); err != nil {
		return -1, err
	}

	l, err := store.Open(dstDir, false)
	if err != nil {
		return -1, err
	}
	defer l.Close()

	apply := folding(l)

	f, err := os.Open(filepath.Join(srcDir, "log.jsonl"))
	if err != nil {
		return -1, err
	}
	defer f.Close()
	applied, skipped := 0, 0
	br := bufio.NewReaderSize(f, 1<<20)
	for offset := int64(0); ; {
		line, rerr := br.ReadString('\n')
		torn := rerr != nil // a line without its newline is the torn tail the log format allows
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			var r record
			if err := json.Unmarshal([]byte(trimmed), &r); err != nil {
				if torn {
					fmt.Fprintf(os.Stderr, "%s: stopping at the torn tail at offset %d\n", id, offset)
					break
				}
				skipped++ // a whole line that is not JSON: the log's reader skipped it, so do we
			} else if err := apply(&r); err != nil {
				return applied, fmt.Errorf("record at offset %d: %w", offset, err)
			} else {
				applied++
			}
		}
		if line != "" {
			offset += int64(len(line))
		}
		if torn {
			break
		}
	}
	if skipped > 0 {
		fmt.Fprintf(os.Stderr, "%s: %d whole lines were not JSON and were skipped\n", id, skipped)
	}
	l.Close()

	if verify {
		if err := verifySession(dstDir); err != nil {
			return applied, err
		}
	}
	return applied, nil
}

// folding returns the fold: one closure that applies records in order and remembers
// which entries are alive, so a patch or delete of something dead stays as silent
// here as it was inert in the log's own fold.
func folding(l *store.Log) func(*record) error {
	alive := map[string]bool{}
	return func(r *record) error {
		switch r.Op {
		case "entry":
			e := r.Entry
			if e == nil || e.ID == "" {
				return fmt.Errorf("entry record %d with no entry", r.Seq)
			}
			if !idShape.MatchString(e.ID) {
				return fmt.Errorf("id %q is not the 7-digit id the log writes; hand-edited logs are refused", e.ID)
			}
			if err := l.AppendEntry(e, r.Tip != "" && r.Tip == e.ID); err != nil {
				return err
			}
			alive[e.ID] = true
		case "patch":
			e := r.Entry
			if e == nil || e.ID == "" {
				return fmt.Errorf("patch record %d with no entry", r.Seq)
			}
			if alive[e.ID] {
				return l.Patch(e)
			}
		case "delete":
			if alive[r.ID] {
				alive[r.ID] = false
				return l.Delete(r.ID)
			}
		case "tip":
			return l.SetTip(r.Tip)
		case "session":
			// session metadata lines: config.json is the live home of those keys now
		default:
			return fmt.Errorf("unknown op %q (a log from a newer build? refuse to guess)", r.Op)
		}
		return nil
	}
}

// verifySession prints one store's aggregates and a payload digest: the corpus
// agrees only if two runs over it agree, and the digest makes "the same entries,
// in order, byte for byte" one comparable number.
func verifySession(dstDir string) error {
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(dstDir, store.DBFileName)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer db.Close()
	var entries, revs, blobs, blobBytes int64
	if err := db.QueryRow(`SELECT
			(SELECT COUNT(*) FROM entry),
			(SELECT COUNT(*) FROM entry_rev),
			(SELECT COUNT(*) FROM blob),
			(SELECT COALESCE(SUM(size), 0) FROM blob)`).Scan(&entries, &revs, &blobs, &blobBytes); err != nil {
		return err
	}
	var tip sql.NullString
	db.QueryRow("SELECT value FROM meta WHERE key = 'tip'").Scan(&tip)
	h := sha256.New()
	rows, err := db.Query("SELECT payload FROM entry ORDER BY seq")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return err
		}
		// why the digest weighs the text and not the bytes: two imports of one corpus
		// must agree whether or not either of them deflated — the scale is the conversation.
		h.Write([]byte(store.PayloadText(payload)))
		h.Write([]byte{'\n'})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	fmt.Printf("verify %s: entries=%d revs=%d blobs=%d blobBytes=%d tip=%s digest=%x\n",
		filepath.Base(dstDir), entries, revs, blobs, blobBytes, tip.String, h.Sum(nil))
	return nil
}

func copyFile(src, dst string, size int64) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	buf := make([]byte, 64<<10)
	for {
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				out.Close()
				return werr
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			out.Close()
			return rerr
		}
	}
	return out.Close()
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// genDictionary trains a zstd dictionary from every payload of every store under
// root and writes it to out. This is the dictionary's whole life support — the seed
// book of a release and a user's personal book are the same command, run by different
// hands on different corpora. Samples come from payloadText, so the dictionary learns
// exactly the shape the payload column will hold, dictionaries included.
func genDictionary(root, out string, maxdict int) {
	dirs, err := storeDirs(root)
	if err != nil {
		fail("%v", err)
	}
	if len(dirs) == 0 {
		fail("no stores (directories holding %s) under %s", store.DBFileName, root)
	}
	// The trainer reads each sample on its own; beyond ~32KB a sample says nothing
	// its head did not already say, and the cap is what keeps a huge corpus fast.
	const sampleCap = 32 << 10
	var samples [][]byte
	var payloadBytes int64
	for _, dir := range dirs {
		db, err := sql.Open("sqlite3", "file:"+filepath.Join(dir, store.DBFileName)+"?mode=ro&_pragma=busy_timeout(5000)")
		if err != nil {
			fail("%s: %v", dir, err)
		}
		rows, err := db.Query("SELECT payload FROM entry ORDER BY seq")
		if err != nil {
			db.Close()
			fail("%s: %v", dir, err)
		}
		n := 0
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				break
			}
			text := store.PayloadText(p)
			payloadBytes += int64(len(text))
			if len(text) > sampleCap {
				text = text[:sampleCap]
			}
			samples = append(samples, []byte(text))
			n++
		}
		rows.Close()
		db.Close()
		fmt.Printf("samples %s: %d payloads\n", dir, n)
	}
	t0 := time.Now()
	d, err := dict.BuildZstdDict(samples, dict.Options{MaxDictSize: maxdict, HashBytes: 6, ZstdLevel: store.ZstdLevel})
	if err != nil {
		fail("train: %v", err)
	}
	if err := os.WriteFile(out, d, 0o644); err != nil {
		fail("%v", err)
	}
	sum := sha256.Sum256(d)
	id := uint32(0)
	if info, err := zstd.InspectDictionary(d); err == nil {
		id = info.ID()
	}
	fmt.Printf("dictionary: %d bytes id=0x%08x from %d samples of %d payload bytes in %v -> %s\n",
		len(d), id, len(samples), payloadBytes, time.Since(t0).Round(time.Millisecond), out)
	fmt.Printf("sha256 %x\n", sum)
	fmt.Println("a dictionary is data: frames it stamped are readable only while this file is kept;")
	fmt.Println("embed it as internal/store/seeddict.bin for the shipped book, or pass -zdict on every run for a personal one")
}

// storeDirs lists the stores under root: root itself if it holds one, else its
// immediate subdirectories that do — the two shapes a daemon data directory and a
// single session directory come in.
func storeDirs(root string) ([]string, error) {
	isStore := func(dir string) bool {
		_, err := os.Stat(filepath.Join(dir, store.DBFileName))
		return err == nil
	}
	if isStore(root) {
		return []string{root}, nil
	}
	var dirs []string
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, de := range entries {
		if de.IsDir() {
			if dir := filepath.Join(root, de.Name()); isStore(dir) {
				dirs = append(dirs, dir)
			}
		}
	}
	return dirs, nil
}
