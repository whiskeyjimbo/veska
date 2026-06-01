// Package application contains use-case orchestrators and application-layer
// services that coordinate the domain entities. Implementations of
// side-effecting ports (storage, parsers, embedding providers) are wired in
// from the infrastructure layer via constructors defined throughout this
// package.
//
// Layout — flat core, sub-packages for satellites: the promotion pipeline
// core lives as loose files directly in this package (ingester, promoter,
// coldscan, resync, scan_tracker, findingemitter, promotion store, errors)
// because those pieces form one cohesive ingestion→promotion flow and share
// the same private seams — the saveFunc/promoteFunc function types and the
// RepoRecord value — which would otherwise force exported plumbing or an
// import cycle if split across sub-packages. Self-contained satellite
// concerns that do NOT touch those shared seams (search, blastradius,
// autolink, contextpack, review, embedder, revalidate, dependencies,
// crossrepo, changedsymbols, extindex, vulnrefresh, wiki, …) each live in
// their own sub-package. Rule of thumb: a file belongs in the flat core if
// it participates in the promotion pipeline's shared seams; otherwise it
// earns a sub-package (solov2-xde2.20).
package application
