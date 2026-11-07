# The zstd dictionary

New zstd payloads are compressed with a **trained dictionary**: a ~32 KB book of the
shapes this store's payloads are made of — the JSON skeleton, the field names, the
shared vocabulary — that every small frame is allowed to reference. Without a book,
each payload is compressed on its own and pays for its own tables; with one, the
tables are already known and the frame only says what differs. At this store's
payload sizes (kilobytes of JSON) that is worth roughly a tenth of the bytes.

It is not magic, and it is not configuration either. **A dictionary is data**: see
[the rules](#the-rules) below.

## What it is worth

Measured on a 17-store corpus (~16 MB of payload text), and on stores the book was
*not* trained on (the honest column):

| | bytes | vs gzip |
|---|---|---|
| gzip | 6,635,884 | — |
| zstd, no dictionary | 6,732,637 | +1.5% |
| zstd + shipped book, stranger's corpus | 5,901,775 | **−11.0%** |
| zstd + book trained on that corpus | 5,777,506 | −12.9% |

Every one of the 17 stores got smaller with a held-out dictionary than without one —
none got worse. Scanning every payload takes ~108 ms zstd against ~205 ms gzip, so
the dictionary buys disk without spending the read path. Training a book from 7.3 MB
of payloads costs ~3 s.

## Where the shipped book comes from

`internal/store/seeddict.bin` is compiled into the binary. It is produced by a person,
committed, and regenerated as a **release act** — never at runtime:

```sh
blimport -gendict -dictout internal/store/seeddict.bin <data-dir>
go build ./...   # the new book is now embedded
```

`-gendict` reads every store under the directory, takes the payloads exactly as the
payload column would hold them, truncates each sample to the first 32 KB (the trainer
learns nothing from more), and prints the book's id, its sha256, and the training
time. Keep that line in the commit message.

Why embed instead of train at runtime: the book's value comes mostly from the schema
skeleton, which is the same for everyone who runs this binary — the part that is
personal (your models, your MCP tools, your prompts) is worth about 2% more, and that
part is what `-zdict` is for.

## Training your own book

```sh
blimport -gendict -dictout my-zdict.bin ~/.bareloop/data     # from a daemon data dir
bareloopd -compress zstd -zdict my-zdict.bin                  # and on every run from now on
```

`-zdict` applies to **new writes only** — existing stores keep the book (or no book)
stamped in their frames, and the reader picks the right one per frame automatically
from the id in the frame header. The books you pass with `-zdict` are registered
*beside* the embedded one, not instead of it; a personal book whose id happens to
equal the shipped one wins on purpose (last registered wins per id).

Re-training is cheap and optional. A stale book is never dangerous — a dictionary
that no longer matches your data just stops helping as much; it cannot corrupt a
read, because every frame is either decodable with its own book or it fails loudly.

## The rules

1. **A frame names the book it was stamped with and needs it forever.** Frames with
   dict_id 0 need none; frames with your id need exactly your file. Deleting or
   rewriting a book you have already written with makes those stores unopenable —
   loudly (the decoder reports an unknown dictionary; nothing is silently
   misdecoded).
2. **A book you have written with is a backup artifact.** Anything that copies
   stores (the archive export, a disk move, a restore) must carry the `-zdict` file
   beside them, the way it carries `config.json`.
3. **Rotating means a new id, and the old book survives.** Old frames never lose
   their stamp; rewriting them onto a newer book is `-recompress`'s job (deferred —
   until it ships, keep every book you ever pointed `-zdict` at).
4. **The shipped book changes only with a release and a commit**, because the
   binary that reads old frames must keep carrying the book they name until every
   store has been rewritten. A book that "improves" every restart is a book that
   silently strands frames.

## What is on disk

A payload column mixes shapes freely and the row's own bytes say which: plain JSON
text (below the 512-byte floor, or when compression lost), gzip, or zstd — with or
without a book. `blimport -verify`'s digest decompresses before hashing, so the same
corpus carries the same digest whichever codec and whichever book wrote it.

## Knobs

- `-maxdict` (blimport): the book's byte budget, default 32768. Bigger books help
  heterogeneous corpora a little and small stores not at all.
- The level is not a knob: the book is trained for the level the store writes with
  (`store.ZstdLevel`), and the trainer tailors the book to it. Training and writing
  cannot drift because they read the same constant.
