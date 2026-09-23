package vpnconfig

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxSubscriptions is how many subscriptions a router keeps: the watch
// downloads them all at once, and the bot's first keyboard lists them all.
const MaxSubscriptions = 10

// MaxSubscriptionName is the longest name, in characters (code points).
const MaxSubscriptionName = 32

// Subscription is one file under <data_dir>/subscriptions: a link, what came of
// its last refresh, and the servers it serves. A subscription without a link
// is a static list the shell imported from a file or a plain-http link, and
// nothing refreshes it. lib/substore.sh reads and writes the same files.
type Subscription struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	URL       string    `json:"url,omitempty"`
	Added     time.Time `json:"added"`
	Refreshed time.Time `json:"refreshed"`
	Error     string    `json:"error,omitempty"`
	Servers   []Server  `json:"servers"`
}

// Static reports a list without a link.
func (s Subscription) Static() bool { return s.URL == "" }

// Host is the host name of the link. Lists and summaries show it in place of
// the link, whose path carries the subscription token. Empty for a static list.
func (s Subscription) Host() string {
	u, err := url.Parse(s.URL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// SubscriptionsDir is where dataDir keeps the subscription files.
func SubscriptionsDir(dataDir string) string {
	return filepath.Join(dataDir, "subscriptions")
}

// ValidSubscriptionID reports an id of 8 lowercase hex digits, the only names
// the store reads or writes.
func ValidSubscriptionID(id string) bool {
	if len(id) != 8 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// LoadSubscriptions reads every <id>.json in dir, ordered by Added, then by id.
// A missing directory holds no subscription. A file named otherwise - the temp
// file of an atomic write among them - is no subscription; a file that does not
// parse, or whose id is not its name, is skipped with a warning rather than
// failing every reader. Each server carries the id of its file.
func LoadSubscriptions(dir string) ([]Subscription, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var subs []Subscription
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !ValidSubscriptionID(id) || e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if errors.Is(err, fs.ErrNotExist) {
			continue // deleted since the listing
		}
		if err != nil {
			return nil, err
		}
		var sub Subscription
		if err := json.Unmarshal(data, &sub); err != nil {
			slog.Warn("Skipping a subscription file that does not parse", "file", e.Name(), "error", err)
			continue
		}
		if sub.ID != id {
			slog.Warn("Skipping a subscription file whose id is not its name", "file", e.Name(), "id", sub.ID)
			continue
		}
		for i := range sub.Servers {
			sub.Servers[i].Subscription = id
		}
		subs = append(subs, sub)
	}
	sort.SliceStable(subs, func(i, j int) bool {
		if !subs[i].Added.Equal(subs[j].Added) {
			return subs[i].Added.Before(subs[j].Added)
		}
		return subs[i].ID < subs[j].ID
	})
	return subs, nil
}

// SaveSubscription writes sub to dir/<id>.json, mode 0600 - the file holds the
// link and every server's credentials - through a temp file and a rename, so a
// reader sees the old file or the new one. The caller holds the config lock.
func SaveSubscription(dir string, sub Subscription) error {
	if !ValidSubscriptionID(sub.ID) {
		return fmt.Errorf("invalid subscription id %q", sub.ID)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(sub, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, sub.ID+".json"), append(data, '\n'))
}

// DeleteSubscriptionFile removes dir/<id>.json. One that is already gone is no
// error. The caller holds the config lock.
func DeleteSubscriptionFile(dir, id string) error {
	if !ValidSubscriptionID(id) {
		return fmt.Errorf("invalid subscription id %q", id)
	}
	if err := os.Remove(filepath.Join(dir, id+".json")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// RemoveLegacyServers deletes dataDir/servers.json, the single list of earlier
// releases. Nothing reads it any more, and every write of a subscription takes
// it away.
func RemoveLegacyServers(dataDir string) error {
	if err := os.Remove(filepath.Join(dataDir, "servers.json")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// NewSubscriptionID draws a random id that no subscription in subs has.
func NewSubscriptionID(subs []Subscription) (string, error) {
	for {
		var b [4]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		id := hex.EncodeToString(b[:])
		if FindSubscription(subs, id) < 0 {
			return id, nil
		}
	}
}

// ErrSubscriptionName is a name the rules refuse; the error says which rule.
var ErrSubscriptionName = errors.New("invalid subscription name")

// CleanSubscriptionName trims the spaces around name and checks the rest: 1 to
// MaxSubscriptionName characters, none of them a control character (C0, DEL,
// C1). lib/substore.sh applies the same rules.
func CleanSubscriptionName(name string) (string, error) {
	name = strings.Trim(name, " ")
	if !utf8.ValidString(name) {
		return "", fmt.Errorf("%w: not UTF-8", ErrSubscriptionName)
	}
	switch n := utf8.RuneCountInString(name); {
	case n == 0:
		return "", fmt.Errorf("%w: empty", ErrSubscriptionName)
	case n > MaxSubscriptionName:
		return "", fmt.Errorf("%w: longer than %d characters", ErrSubscriptionName, MaxSubscriptionName)
	}
	for _, r := range name {
		if isControl(r) {
			return "", fmt.Errorf("%w: it has a control character", ErrSubscriptionName)
		}
	}
	return name, nil
}

func isControl(r rune) bool {
	return r < 0x20 || r == 0x7F || (r >= 0x80 && r <= 0x9F)
}

// foldASCII lower-cases A to Z and nothing else: the fold jq's ascii_downcase
// applies, so the shell and the daemons agree on which names collide.
func foldASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// SubscriptionNameTaken reports whether a subscription other than exceptID
// already has name, under ASCII case folding.
func SubscriptionNameTaken(subs []Subscription, name, exceptID string) bool {
	folded := foldASCII(name)
	for _, s := range subs {
		if s.ID != exceptID && foldASCII(s.Name) == folded {
			return true
		}
	}
	return false
}

// DefaultSubscriptionName is base - a link's host, a file's name - cut to
// MaxSubscriptionName characters, or base-2, base-3 and so on, cut so that the
// suffix fits, whichever no subscription in subs has yet. Control characters
// and the spaces around base go first; an empty base is "subscription".
// lib/substore.sh makes the same choice.
func DefaultSubscriptionName(subs []Subscription, base string) string {
	base = strings.Trim(strings.Map(func(r rune) rune {
		if isControl(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(base, "")), " ")
	if base == "" {
		base = "subscription"
	}
	for n := 1; ; n++ {
		suffix := ""
		if n > 1 {
			suffix = "-" + strconv.Itoa(n)
		}
		name := strings.TrimRight(cutRunes(base, MaxSubscriptionName-utf8.RuneCountInString(suffix)), " ") + suffix
		if !SubscriptionNameTaken(subs, name, "") {
			return name
		}
	}
}

// cutRunes is s cut to its first n characters.
func cutRunes(s string, n int) string {
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

// FindSubscription is the index of the subscription with id in subs, or -1.
func FindSubscription(subs []Subscription, id string) int {
	for i, s := range subs {
		if s.ID == id {
			return i
		}
	}
	return -1
}

// AllServers is every server of every subscription, in subscription order,
// each carrying the id of its subscription.
func AllServers(subs []Subscription) []Server {
	var out []Server
	for _, sub := range subs {
		for _, s := range sub.Servers {
			s.Subscription = sub.ID
			out = append(out, s)
		}
	}
	return out
}

// SubscriptionIPs is xray.servers for subs: every address of every server, the
// set TPROXY_BYPASS takes.
func SubscriptionIPs(subs []Subscription) []string {
	return ServerIPs(AllServers(subs))
}
