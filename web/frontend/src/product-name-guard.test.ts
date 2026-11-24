// @vitest-environment node
/// <reference types="node" />
import { readFileSync, readdirSync, statSync } from "node:fs"
import { basename, join, relative } from "node:path"
import { fileURLToPath } from "node:url"

import { describe, expect, it } from "vitest"

// The WebUI holds no product name: it shows the names the server sends
// (store/product, from /api/auth/status and /api/system/version). The
// product's old name, "claw" (and "ClawEh"), appears nowhere in the frontend:
// not in a label, a storage key, a module name, a test or a comment.
//
// It scans every file under src/ (this one excepted), index.html and the
// text files in public/ for the old name anywhere in a word, in any case.
// Third-party names that contain it are removed first; add one here with a
// comment saying what it names, never the old name itself.
const allowed: RegExp[] = [
  /openclaw/gi, // the OpenClaw device protocol and its openclaw link
  /clawhub/gi, // the ClawHub skills registry (clawhub.ai)
]

const oldName = /claw/i

const root = fileURLToPath(new URL("..", import.meta.url))
const self = fileURLToPath(import.meta.url)

function walk(dir: string, out: string[] = []): string[] {
  for (const entry of readdirSync(dir)) {
    const p = join(dir, entry)
    if (statSync(p).isDirectory()) walk(p, out)
    else out.push(p)
  }
  return out
}

function offenders(file: string, text: string): string[] {
  let cleaned = text
  for (const re of allowed) cleaned = cleaned.replace(re, "")
  return cleaned
    .split("\n")
    .filter((line) => oldName.test(line))
    .map((line) => `${relative(root, file)}: ${line.trim()}`)
}

describe("old name guard", () => {
  it("no file of the frontend names the old name", () => {
    const sources = walk(join(root, "src")).filter(
      (f) => f !== self && !/\.(png|jpe?g|gif|ico|webp|woff2?)$/.test(f),
    )
    // Text files served as they are (public/); images are skipped.
    const publicFiles = walk(join(root, "public")).filter((f) =>
      /\.(webmanifest|json|svg|html|txt|xml)$/.test(f),
    )
    const files = [...sources, ...publicFiles, join(root, "index.html")]
    const found = files.flatMap((f) => [
      ...offenders(f, readFileSync(f, "utf8")),
      ...(oldName.test(basename(f).replace(/openclaw/gi, ""))
        ? [`${relative(root, f)}: file name`]
        : []),
    ])
    expect(found).toEqual([])
  })

  it("catches the old name anywhere in a word", () => {
    expect(offenders("x.tsx", 'const label = "ClawEh on GitHub"')).toHaveLength(
      1,
    )
    expect(offenders("x.tsx", "<span>Claw</span>")).toHaveLength(1)
    expect(
      offenders("x.tsx", 'import { x } from "@/lib/claw-chat-state"'),
    ).toHaveLength(1)
    expect(offenders("x.ts", 'localStorage.getItem("claw:last")')).toHaveLength(
      1,
    )
    expect(offenders("x.ts", "// CLAW_HOME")).toHaveLength(1)
    expect(offenders("x.ts", '"openclaw acp", "https://clawhub.ai"')).toEqual(
      [],
    )
  })
})
