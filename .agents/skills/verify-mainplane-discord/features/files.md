# Files

Files attached to a message go to the session with its text, as one post: the text, with each text file (`text/*`, `application/json`, up to 200 KiB) inlined as `<name>:` and a code block, then one record per image whose type is in the session's `input` (up to 5 MiB). Other files are not sent; the thread gets `-# not sent: <names>`. A message with only files works, and a new thread is named after the first file.

## Sub-features

- `files-image` an image arrives as its own message record (`mainplane tail <id>` shows `message via=discord` with type `image/png`), and the agent can see it.
- `files-text` a text file's content is in the text record.
- `files-skip` an unsupported file is listed as not sent.
- `files-only` a message with files and no text starts or continues a session.

## Driving it with say.sh

- **All three.** Make a tiny PNG with a word in it (`uv run --with pillow python -c ...`), a `notes.txt` holding a word, and a `blob.zip`. In `#bot-test`, send `verify files <run id>: reply with the word in the image and the word in notes.txt` with the three files. The reply names both words; `-# not sent: blob.zip` shows in the thread; `mainplane tail <id>` shows a text record with `notes.txt:` and an `image/png` record.
