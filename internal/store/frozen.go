package store

// FrozenEntry is one entry of a frozen chain: its ref, and the name,
// provider and model it had at the split ("" for an entry that was already
// removed). It never holds a key or a base URL: those come from the
// registry at each dispatch.
type FrozenEntry struct {
	Source   string `json:"source"`
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// FrozenChain is the copy of a reviewer's or the judge's chain that a
// review stores at its split. Fallback and the judge pick read only this
// copy, so a card edit changes only reviews that start after it.
type FrozenChain struct {
	Scope        string        `json:"scope"`
	SavedByAdmin bool          `json:"saved_by_admin"`
	Entries      []FrozenEntry `json:"entries"`
}
