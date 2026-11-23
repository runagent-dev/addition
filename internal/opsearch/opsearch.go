// Package opsearch ranks the imported API operations against a plain-language query, offline.
package opsearch

import (
	"cmp"
	"math"
	"slices"
	"strings"

	"github.com/runagent-dev/addition/internal/catalog"
)

// Entry is one operation and the app (provider id) it belongs to.
type Entry struct {
	App string
	Op  catalog.Operation
}

// Hit is an Entry with its rank score.
type Hit struct {
	Entry
	Score float64
}

// Options narrow a search. A zero Limit returns every match.
type Options struct {
	App    string
	Effect catalog.Effect
	Limit  int
}

// AppSummary counts an app's operations by effect.
type AppSummary struct {
	App        string
	Operations int
	Effects    map[catalog.Effect]int
}

const (
	k1 = 1.2
	b  = 0.75

	exactIDBonus = 1000
	// relevanceFloor drops matches scoring under this share of the best one, so `total` means something.
	relevanceFloor = 0.4
	synonymWeight  = 0.5
	// softSynonymWeight is for the expansion of a soft word: the app it names is the real signal.
	softSynonymWeight = 0.8
)

// Field weights: what an operation is called says more than how it is described, and the last
// two id segments (resource and verb, as in messages.send) say more than the API prefix.
const (
	verbWeight     = 4.0
	resourceWeight = 2.5
	idWeight       = 1.0
	appWeight      = 2.0
	summaryWeight  = 1.0
	paramWeight    = 1.0
)

// Index is a BM25 index over operations. It is read-only after New.
type Index struct {
	entries []Entry
	terms   []map[string]float64
	length  []float64
	avgLen  float64
	df      map[string]int
	byID    map[string]int
}

// New indexes every operation of sets.
func New(sets []*catalog.OperationSet) *Index {
	ix := &Index{df: map[string]int{}, byID: map[string]int{}}
	for _, set := range sets {
		for _, op := range set.Operations {
			ix.entries = append(ix.entries, Entry{App: set.Provider, Op: op})
		}
	}
	slices.SortFunc(ix.entries, func(a, c Entry) int { return cmp.Compare(a.Op.ID, c.Op.ID) })
	var total float64
	for i, e := range ix.entries {
		tf := documentTerms(e)
		var length float64
		for term, w := range tf {
			length += w
			ix.df[term]++
		}
		ix.terms = append(ix.terms, tf)
		ix.length = append(ix.length, length)
		ix.byID[e.Op.ID] = i
		total += length
	}
	if len(ix.entries) > 0 {
		ix.avgLen = total / float64(len(ix.entries))
	}
	return ix
}

func documentTerms(e Entry) map[string]float64 {
	tf := map[string]float64{}
	add := func(text string, weight float64) {
		for _, t := range tokenize(text) {
			tf[t] += weight
		}
	}
	segments := strings.Split(e.Op.ID, ".")
	for i, seg := range segments {
		switch {
		case i == len(segments)-1:
			add(seg, verbWeight)
		case i == len(segments)-2:
			add(seg, resourceWeight)
		default:
			add(seg, idWeight)
		}
	}
	add(e.App, appWeight)
	add(e.Op.Summary, summaryWeight)
	for _, p := range e.Op.Params {
		add(p.Name, paramWeight)
	}
	return tf
}

// Search returns the best matches for query, best first, and how many operations matched in all.
func (ix *Index) Search(query string, opt Options) ([]Hit, int) {
	terms := queryTerms(query)
	lowered := strings.ToLower(strings.TrimSpace(query))
	var hits []Hit
	for i, e := range ix.entries {
		if opt.App != "" && e.App != opt.App || opt.Effect != "" && e.Op.Effect != opt.Effect {
			continue
		}
		score := ix.score(i, terms)
		if lowered != "" && (strings.ToLower(e.Op.ID) == lowered || strings.HasSuffix(strings.ToLower(e.Op.ID), "."+lowered)) {
			score += exactIDBonus
		}
		if score > 0 {
			hits = append(hits, Hit{Entry: e, Score: score})
		}
	}
	slices.SortFunc(hits, func(a, c Hit) int {
		return cmp.Or(-cmp.Compare(a.Score, c.Score), cmp.Compare(a.Op.ID, c.Op.ID))
	})
	if len(hits) > 0 && hits[0].Score < exactIDBonus {
		cut := slices.IndexFunc(hits, func(h Hit) bool { return h.Score < hits[0].Score*relevanceFloor })
		if cut >= 0 {
			hits = hits[:cut]
		}
	}
	total := len(hits)
	if opt.Limit > 0 && len(hits) > opt.Limit {
		hits = hits[:opt.Limit]
	}
	return hits, total
}

func (ix *Index) score(doc int, terms map[string]float64) float64 {
	n := float64(len(ix.entries))
	var score float64
	for term, qw := range terms {
		tf := ix.terms[doc][term]
		if tf == 0 {
			continue
		}
		df := float64(ix.df[term])
		idf := math.Log(1 + (n-df+0.5)/(df+0.5))
		norm := tf * (k1 + 1) / (tf + k1*(1-b+b*ix.length[doc]/ix.avgLen))
		score += qw * idf * norm
	}
	return score
}

// Find looks an operation up by its exact id.
func (ix *Index) Find(id string) (Entry, bool) {
	i, ok := ix.byID[id]
	if !ok {
		return Entry{}, false
	}
	return ix.entries[i], true
}

// Nearest lists up to n operation ids closest to id by spelling, for "did you mean".
func (ix *Index) Nearest(id string, n int) []string {
	want := strings.ToLower(id)
	type scored struct {
		id   string
		dist int
	}
	all := make([]scored, 0, len(ix.entries))
	for _, e := range ix.entries {
		all = append(all, scored{e.Op.ID, editDistance(want, strings.ToLower(e.Op.ID))})
	}
	slices.SortFunc(all, func(a, c scored) int { return cmp.Or(cmp.Compare(a.dist, c.dist), cmp.Compare(a.id, c.id)) })
	var out []string
	for i := 0; i < len(all) && i < n; i++ {
		out = append(out, all[i].id)
	}
	return out
}

// Apps counts operations per app, sorted by app id.
func (ix *Index) Apps() []AppSummary {
	byApp := map[string]*AppSummary{}
	for _, e := range ix.entries {
		s := byApp[e.App]
		if s == nil {
			s = &AppSummary{App: e.App, Effects: map[catalog.Effect]int{}}
			byApp[e.App] = s
		}
		s.Operations++
		s.Effects[e.Op.Effect]++
	}
	out := make([]AppSummary, 0, len(byApp))
	for _, s := range byApp {
		out = append(out, *s)
	}
	slices.SortFunc(out, func(a, c AppSummary) int { return cmp.Compare(a.App, c.App) })
	return out
}

func editDistance(a, c string) int {
	prev := make([]int, len(c)+1)
	cur := make([]int, len(c)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(c); j++ {
			cost := 1
			if a[i-1] == c[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(c)]
}
