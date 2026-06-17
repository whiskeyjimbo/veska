// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package synthcorpus

import (
	"fmt"
	"strings"
)

// semanticTopics holds the per-cluster topic vocabularies used by
// GenerateSemanticCorpus. Each topic is a bag of distinct,
// domain-coherent phrases; node text is assembled by sampling several
// phrases from one bag.
// The design goal is a corpus that real embedding models (nomic-embed-text
// and similar) cluster the way the structural ground truth says they
// should. The earlier {verb} {entity} template failed because every
// cluster shared the same verb/modifier scaffold, so cross-cluster pairs
// at the same template slot ("validate user …" vs "validate account …")
// scored as high as within-cluster pairs. The fix here is that the
// vocabularies are *disjoint across clusters*: a cross-cluster pair shares
// no phrases at all, so its embedding similarity stays well below a
// within-cluster pair's. A probe of nomic-embed-text on this design
// measured within-cluster cosine mean ≈ 0.74 vs between-cluster ≈ 0.49.
// Topics deliberately include semantically adjacent neighbours
// (authentication/permissions/encryption, payments/tax/shipping,
// caching/database) so the auto-link false-positive measurement still
// sees realistic cross-cluster confusion rather than a trivially
// separable dataset.
// Each bag has exactly semanticPhrasesPerTopic entries so the
// nth-combination addressing in GenerateSemanticCorpus is uniform.
var semanticTopics = []struct {
	name    string
	phrases []string
}{
	{"authentication", []string{
		"user login", "session logout", "password reset", "session token expiry",
		"multi-factor authentication", "oauth authorization flow", "credential storage",
		"single sign-on", "biometric unlock", "failed login attempt", "account lockout",
		"authentication challenge",
	}},
	{"caching", []string{
		"cache eviction", "time-to-live expiry", "least-recently-used policy",
		"cache warm-up", "stale cache entry", "cache invalidation", "write-through cache",
		"cache hit ratio", "distributed cache node", "cache miss penalty",
		"in-memory cache size", "cache key namespace",
	}},
	{"payments", []string{
		"credit card charge", "payment refund", "payment gateway",
		"transaction settlement", "currency conversion", "chargeback dispute",
		"billing cycle", "payment processor", "fraud risk score", "recurring subscription",
		"invoice total", "declined payment",
	}},
	{"search-indexing", []string{
		"inverted index", "full-text query", "relevance ranking", "text tokenizer",
		"stop word removal", "faceted search filter", "autocomplete suggestion",
		"search latency budget", "index shard", "query parser", "document boost",
		"search result snippet",
	}},
	{"message-queue", []string{
		"message broker", "topic partition", "consumer group", "dead letter queue",
		"message acknowledgement", "consumer backpressure", "at-least-once delivery",
		"queue depth", "event publication", "offset commit", "message redelivery",
		"poison message",
	}},
	{"logging", []string{
		"log level filter", "structured log record", "log rotation", "log aggregation",
		"trace span", "correlation identifier", "error stack trace", "audit trail entry",
		"log shipping pipeline", "debug output", "log sampling rate", "log retention window",
	}},
	{"database", []string{
		"sql query plan", "table schema", "foreign key constraint", "transaction commit",
		"connection pool", "row-level lock", "schema migration", "index scan",
		"replication lag", "deadlock detection", "prepared statement", "vacuum reclaim",
	}},
	{"networking", []string{
		"tcp socket", "http request", "dns lookup", "load balancer pool",
		"packet loss", "round-trip latency", "connection timeout", "tls handshake",
		"reverse proxy route", "bandwidth throttle", "keep-alive connection", "ip allowlist",
	}},
	{"file-storage", []string{
		"blob upload", "object store bucket", "bucket access policy", "file checksum",
		"storage tier", "multipart upload", "presigned download url", "object retention policy",
		"cold archive storage", "file metadata", "directory listing", "byte-range read",
	}},
	{"email", []string{
		"smtp delivery", "email template", "bounce handling", "spam filter score",
		"mailing list", "unsubscribe link", "inbox folder", "attachment size limit",
		"dkim signature", "email open rate", "deliverability reputation", "html email body",
	}},
	{"scheduling", []string{
		"cron job", "task interval", "recurring calendar event", "time zone offset",
		"calendar slot booking", "deadline reminder", "schedule conflict",
		"delayed job runner", "next fire time", "missed run catch-up", "blackout window",
		"jitter delay",
	}},
	{"monitoring", []string{
		"metric gauge", "alert threshold", "dashboard panel", "service health check",
		"uptime probe", "anomaly detection", "incident page", "sla breach",
		"latency percentile", "error budget burn", "synthetic check", "saturation signal",
	}},
	{"permissions", []string{
		"access control list", "role assignment", "permission grant", "policy rule",
		"scope check", "privilege escalation", "resource owner", "deny rule",
		"admin override", "least-privilege default", "group membership", "permission audit",
	}},
	{"user-profile", []string{
		"display name", "avatar image", "profile biography", "profile settings",
		"account preferences", "contact information", "profile visibility",
		"language locale", "interface theme choice", "notification opt-in",
		"profile completion", "vanity url handle",
	}},
	{"shopping-cart", []string{
		"add to cart", "cart subtotal", "checkout flow", "line item quantity",
		"remove cart item", "saved cart", "cart abandonment", "promo code entry",
		"shipping estimate", "cart expiry", "guest checkout", "cart merge on login",
	}},
	{"inventory", []string{
		"stock level", "restock purchase order", "warehouse bin location", "sku quantity count",
		"inventory audit", "low stock alert", "backorder queue", "stock reservation",
		"supplier lead time", "inventory shrinkage", "cycle count", "stock transfer",
	}},
	{"analytics", []string{
		"page view event", "event tracking", "funnel conversion rate", "cohort retention",
		"session duration", "bounce rate", "attribution model", "audience segment filter",
		"dashboard report", "click event", "active user count", "engagement metric",
	}},
	{"video-streaming", []string{
		"video transcode", "bitrate ladder", "playback buffer", "adaptive streaming",
		"video codec", "thumbnail frame", "stream manifest", "watch time",
		"resolution switch", "cdn edge cache", "live stream ingest", "dropped frame",
	}},
	{"machine-learning", []string{
		"model training run", "feature vector", "gradient descent step", "training epoch",
		"validation loss", "hyperparameter sweep", "inference latency", "model checkpoint",
		"dataset train-test split", "prediction confidence score", "overfitting regularization",
		"embedding layer",
	}},
	{"version-control", []string{
		"git commit", "branch merge", "pull request review", "merge conflict",
		"code diff hunk", "commit history", "repository clone", "release tag",
		"interactive rebase", "staged changeset", "detached head", "cherry-pick",
	}},
	{"deployment", []string{
		"container image build", "rolling update", "blue-green deployment", "release rollback",
		"deployment pipeline", "build artifact", "environment configuration", "canary release",
		"health gate", "post-deploy hook", "infrastructure provisioning", "version pinning",
	}},
	{"encryption", []string{
		"symmetric cipher", "public-private key pair", "encryption at rest", "key rotation",
		"hash digest", "password salt value", "certificate chain", "payload decryption",
		"tls cipher suite", "secret vault", "envelope encryption", "nonce reuse",
	}},
	{"geolocation", []string{
		"latitude and longitude", "map marker", "geofence boundary", "search radius distance",
		"address geocoding", "route planning", "gps coordinate", "location pin drop",
		"region polygon", "nearby place search", "reverse geocode", "elevation lookup",
	}},
	{"chat-messaging", []string{
		"chat room", "direct message", "typing indicator", "message thread reply",
		"read receipt", "online presence status", "group chat channel", "message history scroll",
		"emoji reaction", "mention notification", "message edit", "pinned message",
	}},
	{"recommendation", []string{
		"recommended item", "similar product", "collaborative filtering", "user preference profile",
		"popularity ranking", "personalized feed", "item affinity score", "recommendation slate",
		"cold-start problem", "click feedback signal", "trending now", "diversity re-ranking",
	}},
	{"tax", []string{
		"tax rate lookup", "taxable amount", "tax exemption", "sales tax",
		"value-added tax calculation", "tax jurisdiction", "tax filing", "withholding amount",
		"tax bracket", "itemized deduction", "tax-inclusive price", "nexus determination",
	}},
	{"shipping", []string{
		"shipment tracking number", "delivery date estimate", "carrier shipping label",
		"package weight", "shipping zone", "freight cost", "customs declaration",
		"prepaid return label", "warehouse dispatch", "last-mile delivery",
		"signature on delivery", "split shipment",
	}},
	{"localization", []string{
		"language translation string", "locale number format", "currency symbol",
		"date format pattern", "right-to-left text layout", "translation catalog",
		"regional setting", "pluralization rule", "character encoding", "translated content",
		"fallback locale", "machine translation review",
	}},
	{"rate-limiting", []string{
		"request quota", "throttle policy", "rate limit window", "burst allowance",
		"token bucket refill", "quota exceeded response", "exponential backoff retry",
		"per-client api limit", "sliding window counter", "concurrency cap",
		"rate limit header", "leaky bucket drain",
	}},
	{"backup-recovery", []string{
		"data backup job", "snapshot restore", "recovery point objective", "backup schedule",
		"disaster recovery plan", "incremental backup", "restore drill test", "retention window",
		"backup integrity verification", "point-in-time recovery", "offsite backup copy",
		"recovery time objective",
	}},
}

const (
	// semanticPhrasesPerTopic is the size of every topic's phrase bag.
	semanticPhrasesPerTopic = 12

	// semanticPhrasesPerNode is how many phrases are sampled (without
	// repetition) into one node's text. 5 of 12 gives C(12,5)=792
	// distinct nodes per cluster.
	semanticPhrasesPerNode = 5
)

// SemanticClusterCount is the number of clusters the semantic corpus
// provides - fixed by the hand-authored topic vocabulary. Harnesses must
// use this rather than assuming a cluster count.
var SemanticClusterCount = len(semanticTopics)

// semanticNodesPerClusterCap is the maximum distinct nodes one cluster
// can yield: C(semanticPhrasesPerTopic, semanticPhrasesPerNode).
var semanticNodesPerClusterCap = binomial(semanticPhrasesPerTopic, semanticPhrasesPerNode)

// GenerateSemanticCorpus builds a deterministic corpus tailored for real
// embedding models. Each cluster draws from one hand-authored topic
// vocabulary (see semanticTopics); a node's text is the j-th
// semanticPhrasesPerNode-combination of that topic's phrase bag, joined
// into a short descriptive blob.
// The cluster count is fixed at SemanticClusterCount; only nodesPerCluster
// is a parameter, and it must not exceed semanticNodesPerClusterCap.
// The Corpus shape (Nodes, CenterQueries, TruthByCluster, ClusterOf) is
// identical to GenerateCorpus so the recall and autolink harnesses can
// substitute one generator for the other.
func GenerateSemanticCorpus(nodesPerCluster int) Corpus {
	if nodesPerCluster < 1 {
		panic(fmt.Sprintf("synthcorpus: GenerateSemanticCorpus: nodesPerCluster=%d must be >= 1", nodesPerCluster))
	}
	if nodesPerCluster > semanticNodesPerClusterCap {
		panic(fmt.Sprintf("synthcorpus: GenerateSemanticCorpus: nodesPerCluster=%d exceeds capacity %d (C(%d,%d))",
			nodesPerCluster, semanticNodesPerClusterCap, semanticPhrasesPerTopic, semanticPhrasesPerNode))
	}

	clusters := len(semanticTopics)
	c := Corpus{
		Clusters:        clusters,
		NodesPerCluster: nodesPerCluster,
		Nodes:           make([]SyntheticNode, 0, clusters*nodesPerCluster),
		CenterQueries:   make([]string, clusters),
	}
	for k, topic := range semanticTopics {
		// Center query: the first semanticPhrasesPerNode phrases of the
		// bag - a strong, representative anchor for the recall harness.
		c.CenterQueries[k] = strings.Join(topic.phrases[:semanticPhrasesPerNode], ". ") + "."
		for j := range nodesPerCluster {
			pick := nthCombination(j, semanticPhrasesPerTopic, semanticPhrasesPerNode)
			parts := make([]string, len(pick))
			for i, idx := range pick {
				parts[i] = topic.phrases[idx]
			}
			c.Nodes = append(c.Nodes, SyntheticNode{
				NodeID:     fmt.Sprintf("sem_c%d_n%d", k, j),
				Cluster:    k,
				Text:       strings.Join(parts, ". ") + ".",
				SymbolPath: fmt.Sprintf("semantic.%s.n%d", topic.name, j),
				FilePath:   fmt.Sprintf("semantic/%s.go", topic.name),
				Kind:       "function",
			})
		}
	}
	return c
}

// binomial returns C(n, k) for small n, k. Panics on negative input.
func binomial(n, k int) int {
	if k < 0 || n < 0 || k > n {
		return 0
	}
	if k > n-k {
		k = n - k
	}
	result := 1
	for i := range k {
		result = result * (n - i) / (i + 1)
	}
	return result
}

// nthCombination returns the n-th k-combination of {0,1,…,total-1} in
// lexicographic order, as a sorted, ascending int slice. n is taken
// modulo C(total, k) so callers can pass any non-negative index.
// Standard combinatorial unranking: walk the candidate values, and for
// each one decide whether it belongs in the result by comparing n against
// the number of combinations that would skip it.
func nthCombination(n, total, k int) []int {
	count := binomial(total, k)
	n %= count

	out := make([]int, 0, k)
	value := 0
	for k > 0 {
		// Combinations of the remaining (total-1-value) values taken
		// (k-1) at a time - i.e. those that include `value`.
		c := binomial(total-1-value, k-1)
		if n < c {
			out = append(out, value)
			k--
		} else {
			n -= c
		}
		value++
	}
	return out
}
