# Third-party notices

Kiro-Go is MIT-licensed (see `LICENSE`). The Token Optimization feature embeds
unmodified prompt text from two open-source projects. Each file is a
byte-for-byte copy under `tokenopt/assets/`, pinned and hash-checked by
`tokenopt/assets/manifest.json`; the license texts ship beside them and are
compiled into the binary.

| Asset | Project | Version | License | License file |
|---|---|---|---|---|
| `caveman/caveman-activate.md` (upstream `src/rules/caveman-activate.md`) | [JuliusBrussee/caveman](https://github.com/JuliusBrussee/caveman) | v4.0.1 (`d262ab3882952a59312ff3537cd86d894523c5ab`) | Apache-2.0 | `tokenopt/assets/caveman/LICENSE`, `NOTICE`, `LICENSE-MIT` |
| `caveman/ultracave-SKILL.md` (upstream `skills/ultracave/SKILL.md`) | JuliusBrussee/caveman | v4.0.1 | Apache-2.0 | same |
| `ponytail/SKILL.md` (upstream `skills/ponytail/SKILL.md`) | [DietrichGebert/ponytail](https://github.com/DietrichGebert/ponytail) | v5.3.2 (`16352ac229eb08a9598dcae48fe9f2b342567d86`) | MIT | `tokenopt/assets/ponytail/LICENSE` |

## Notes

- **Apache-2.0 (Caveman).** The files are redistributed unmodified, with the
  license and `NOTICE` retained. At request time the YAML frontmatter of the
  ultracave skill is dropped, trailing whitespace is trimmed, and the text is
  appended to a prompt; the vendored files themselves are not edited. Caveman's `LICENSE-MIT` is kept because its
  `NOTICE` says code contributed before 3.0.0 remains available under MIT.
- **Trademark.** "Caveman" is a trademark of its author; the license grants no
  trademark rights. Kiro-Go refers to it only to say what text it applies, and
  is not affiliated with or endorsed by the project.
- **MIT (Ponytail).** The copyright and permission notice travel with the file.
- **Ponytail level selection** is a Go port of the selection logic in the
  project's `hooks/ponytail-instructions.js`; its output is pinned byte-for-byte
  by a golden test generated from that script.
- **Not bundled.** Headroom (Apache-2.0) and RTK (Apache-2.0 + BUSL-1.1) are
  separate programs the operator installs and runs; Kiro-Go ships none of their
  code and executes none of their binaries.
