---
name: solov2 is a separate git repo
description: solov2 is a nested git repo inside engram — all solov2 code commits go to solov2's git, not engram's
type: feedback
---

solov2 (/home/jrose/src/engram/solov2) has its own git repository (.git at /home/jrose/src/engram/solov2/.git) and its own go.mod (module github.com/whiskeyjimbo/engram/solov2). All spike/feature code for solov2 must be committed to solov2's git — never to the parent engram module.

**Why:** solov2 is a separate project (Solo V2 design) that lives inside the engram monorepo directory but is tracked independently. Committing solov2 code to the engram go.mod pollutes the parent module with spike/throwaway deps.

**How to apply:** When working on any solov2-r3x or solov2-* issues, all file writes and git commits go inside /home/jrose/src/engram/solov2/. Run `git` commands from /home/jrose/src/engram/solov2/ (not from /home/jrose/src/engram/). The go module is github.com/whiskeyjimbo/engram/solov2 with go.mod at /home/jrose/src/engram/solov2/go.mod.
