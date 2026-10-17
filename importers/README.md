# importers

Not used. Bank, card, brokerage and mortgage data comes from the Finance connector. It is
staged and booked by `finance import connector` (see `internal/connector`, `internal/books`,
`internal/ledger`). A new source should get a reader that produces `connector.Extract`-like
rows and staging entries. Do not hand-write journal entries.
