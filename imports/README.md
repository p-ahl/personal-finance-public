# imports

Raw financial-institution downloads, organized as:

```text
imports/YYYY-MM-DD/<fi>/*.json
```

JSON payloads are gitignored; only this README and `.gitkeep` are tracked.
Downloaders write atomically and skip unchanged checksums.
