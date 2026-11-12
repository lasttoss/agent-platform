package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// Three layers, ported from gateway/cache.py:
//
//	L1 exact  — same normalised request, same tool set, same policy version → no model call
//	L2 semantic — reworded question in the same scope                        → reuse the answer
//	L3 prefix — stable prompt prefix at the provider                         → billed once
//
// One deliberate difference: Python persists to SQLite, Go keeps this in memory, so a restart
// loses the cache but never loses the audit log. Durability of cheap derived data is not worth
// a database here; if it ever is, the key formula stays the same.
const (
	EmbedDim          = 256
	SemanticThreshold = 0.93
)

type CacheEntry struct {
	Answer  string
	Saved   int
	Created time.Time
	TTL     time.Duration // 0 = do not cache
}

type SemanticEntry struct {
	Scope   string
	Text    string
	Vec     []float64
	Answer  string
	Saved   int
	Created time.Time
	TTL     time.Duration
}

type Cache struct {
	mu       sync.Mutex
	exact    map[string]CacheEntry
	semantic []SemanticEntry
	stats    map[string]int
}

func NewCache() *Cache {
	return &Cache{exact: map[string]CacheEntry{}, stats: map[string]int{}}
}

func Normalise(s string) string { return Fold(strings.TrimSpace(s)) }

func hashKey(payload any) string {
	raw, _ := json.Marshal(payload)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ExactKey: tool order must not change the key, and scope must (a personal answer is stored
// under its user and never served to another one).
func ExactKey(text string, tools []string, policyVersion, chatType, scope string) string {
	sorted := append([]string{}, tools...)
	sort.Strings(sorted)
	return hashKey(map[string]any{"text": Normalise(text), "tools": sorted,
		"policy": policyVersion, "chat": chatType, "scope": scope})
}

// PrefixFingerprint is L3: the part of the prompt that has to stay byte-identical for the
// provider's prefix cache to keep paying off.
func PrefixFingerprint(tools []string, policyVersion string) string {
	sorted := append([]string{}, tools...)
	sort.Strings(sorted)
	return hashKey(map[string]any{"tools": sorted, "policy": policyVersion})
}

func Embed(text string) []float64 {
	vec := make([]float64, EmbedDim)
	for _, tok := range strings.FieldsFunc(Normalise(text), func(r rune) bool {
		return !(r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z')
	}) {
		h := 0
		for _, r := range tok {
			h = (h*31 + int(r)) % EmbedDim
		}
		vec[h]++
	}
	return vec
}

func Cosine(a, b []float64) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func (c *Cache) bump(kind string) {
	c.stats[kind]++
}

func (c *Cache) GetExact(key string) (CacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.exact[key]
	if !ok {
		c.bump("l1_miss")
		return CacheEntry{}, false
	}
	if e.TTL > 0 && time.Since(e.Created) > e.TTL {
		delete(c.exact, key)
		c.bump("l1_expired")
		return CacheEntry{}, false
	}
	c.bump("l1_hit")
	return e, true
}

func (c *Cache) PutExact(key string, e CacheEntry) {
	if e.TTL == 0 { // TTL 0 means "never cache"
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e.Created = time.Now()
	c.exact[key] = e
}

func (c *Cache) GetSemantic(text, scope string) (SemanticEntry, float64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	vec := Embed(text)
	best, bestSim := SemanticEntry{}, 0.0
	kept := c.semantic[:0]
	for _, e := range c.semantic {
		if e.TTL > 0 && time.Since(e.Created) > e.TTL {
			continue
		}
		kept = append(kept, e)
		if e.Scope != scope {
			continue
		}
		if sim := Cosine(vec, e.Vec); sim > bestSim {
			best, bestSim = e, sim
		}
	}
	c.semantic = kept
	if best.Answer != "" && bestSim >= SemanticThreshold {
		c.bump("l2_hit")
		return best, bestSim, true
	}
	c.bump("l2_miss")
	return SemanticEntry{}, bestSim, false
}

func (c *Cache) PutSemantic(text, answer string, saved int, ttl time.Duration, scope string) {
	if ttl == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.semantic = append(c.semantic, SemanticEntry{Scope: scope, Text: Normalise(text),
		Vec: Embed(text), Answer: answer, Saved: saved, Created: time.Now(), TTL: ttl})
}

func (c *Cache) Stats() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	rate := func(hit, miss string) float64 {
		total := c.stats[hit] + c.stats[miss]
		if total == 0 {
			return 0
		}
		return float64(c.stats[hit]) / float64(total)
	}
	return map[string]any{
		"l1_hit": c.stats["l1_hit"], "l1_hit_rate": rate("l1_hit", "l1_miss"),
		"l2_hit": c.stats["l2_hit"], "l2_hit_rate": rate("l2_hit", "l2_miss"),
	}
}
