#!/usr/bin/env python3
"""Draw the Silo Platters mark into an SF Symbols template.

macOS does not take an image for the Finder sidebar icon; it takes an SF Symbol
name in the File Provider extension's Info.plist. To use our own mark it has to
become a custom symbol, which means an SF Symbols template SVG with our paths in
place of the ones the template ships with.

Apple's guide coordinates differ between template versions, so this reads the
Capline/Baseline guides out of the template you hand it rather than assuming a
cap height. Export a template from the SF Symbols app (File > Export Template,
static or variable), run this over it, then validate it back in SF Symbols or by
dropping it into an Xcode asset catalog.

    ./make-symbol.py Silo.svg -o SiloSymbol.svg

Every <g id="Weight-Scale"> the template contains is filled in; a variable
template has the three interpolation sources and the system generates the rest.
All variants get three four-point paths in the same order, which is what makes
the template interpolatable and keeps annotations in sync.
"""

import argparse
import re
import sys

# Bar thickness is set indirectly, as the gap between bars, so that three bars
# plus two gaps always sum to exactly the cap height. Heavier weight, tighter gap.
GAP_FRACTION = {
    "Ultralight": 0.190,
    "Thin": 0.160,
    "Light": 0.115,
    "Regular": 0.0714,  # the 12-on-42 proportion the 64px mark is drawn at
    "Medium": 0.060,
    "Semibold": 0.050,
    "Bold": 0.042,
    "Heavy": 0.035,
    "Black": 0.028,
}

WIDTH_RATIO = 44 / 42   # mark width over mark height, from the 64 artboard
INSET_RATIO = 12 / 42   # how far the middle bar is inset from the left

VARIANT_RE = re.compile(
    r'(<g\s+id="(' + "|".join(GAP_FRACTION) + r')-([SML])"[^>]*>)(.*?)(</g>)',
    re.DOTALL,
)


def guide(svg, name):
    """y of a named guide line, e.g. Capline-S."""
    m = re.search(r'<line\s+id="%s"[^>]*?\sy1="([-\d.]+)"' % re.escape(name), svg)
    if not m:
        m = re.search(r'<line[^>]*?\sy1="([-\d.]+)"[^>]*?\sid="%s"' % re.escape(name), svg)
    if not m:
        raise SystemExit("no %s guide in the template — is this an SF Symbols template?" % name)
    return float(m.group(1))


def platters(cap, weight):
    """Three bars filling `cap` vertically, on a baseline at y=0 (so y is negative)."""
    gap = cap * GAP_FRACTION[weight]
    bar = (cap - 2 * gap) / 3
    width = cap * WIDTH_RATIO
    inset = cap * INSET_RATIO
    out = []
    for i, x in enumerate((0.0, inset, 0.0)):
        top = -cap + i * (bar + gap)
        out.append(
            '<path d="M%.4f %.4fL%.4f %.4fL%.4f %.4fL%.4f %.4fZ"/>'
            % (x, top, width, top, width, top + bar, x, top + bar)
        )
    return "".join(out)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("template", help="SVG exported from SF Symbols")
    ap.add_argument("-o", "--output", default="-", help="output SVG (default: stdout)")
    args = ap.parse_args()

    with open(args.template) as f:
        svg = f.read()

    caps = {s: abs(guide(svg, "Baseline-" + s) - guide(svg, "Capline-" + s)) for s in "SML"}

    filled = []

    def replace(m):
        head, weight, scale, _body, tail = m.groups()
        filled.append("%s-%s" % (weight, scale))
        return head + platters(caps[scale], weight) + tail

    svg, n = VARIANT_RE.subn(replace, svg)
    if not n:
        raise SystemExit("no <g id=\"Weight-Scale\"> variants found — nothing to draw into")

    if args.output == "-":
        sys.stdout.write(svg)
    else:
        with open(args.output, "w") as f:
            f.write(svg)
    print("drew %d variants: %s" % (n, ", ".join(filled)), file=sys.stderr)
    print("cap heights: " + ", ".join("%s=%.2f" % (s, caps[s]) for s in "SML"), file=sys.stderr)


if __name__ == "__main__":
    main()
