package dsqlgrant

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// ItemVersion is a short hash of a roles-table item's contents, stored in the
// item's "version" attribute. The bootstrap Lambda copies it into the item's
// status, so `anvil deploy` can tell a status written for this version of the
// item from one left by an earlier deploy. Unchanged contents give the same
// version, so an unchanged item isn't rewritten (and fires no stream event).
// json.Marshal sorts map keys, so the hash doesn't depend on field order.
func ItemVersion(item interface{}) string {
	b, _ := json.Marshal(item)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}
