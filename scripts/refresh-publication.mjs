#!/usr/bin/env node
// Recomputes PUBLICATION.json's file map from the tracked tree.
//
// The publication manifest pins the exact bytes of every published file, and the
// byte-policy gate refuses a tree whose manifest disagrees with the files. Adding
// or editing a document therefore requires refreshing the map in the same change.
//
// It only rewrites entries: metadata such as the acceptance boundary is left to
// a human, because that is a claim, not a measurement.
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";

const root = join(import.meta.dirname, "..");
const manifestPath = join(root, "PUBLICATION.json");
const raw = readFileSync(manifestPath, "utf8");
const manifest = JSON.parse(raw);

const tracked = execFileSync("git", ["ls-files", "-z"], { cwd: root, encoding: "utf8" })
  .split("\0")
  .filter(Boolean)
  .filter((p) => p !== "PUBLICATION.json");

const files = {};
for (const path of tracked.sort()) {
  const bytes = readFileSync(join(root, path));
  files[path] = { bytes: bytes.length, sha256: createHash("sha256").update(bytes).digest("hex") };
}

const before = Object.keys(manifest.files).length;
manifest.files = files;
writeFileSync(manifestPath, JSON.stringify(manifest, null, 2) + "\n", "utf8");
console.log(`publication manifest: ${before} -> ${Object.keys(files).length} files`);
