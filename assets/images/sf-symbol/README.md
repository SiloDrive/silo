# Finder sidebar icon

macOS does not take an image for a File Provider extension's sidebar icon. It
takes the *name of an SF Symbol*, set in the extension's `Info.plist`
([Setting the Finder Sidebar Icon][doc]):

```xml
<key>CFBundleIcons</key>
<dict>
    <key>CFBundlePrimaryIcon</key>
    <dict>
        <key>CFBundleSymbolName</key>
        <string>Silo</string>
    </dict>
</dict>
```

That key goes in the **extension** bundle's `Info.plist`, not the host app's.
There is no PNG slot and no size ladder — `../silo-sidebar-16*.svg` is useful for
the web and for a menu-bar item, but it is not what Finder reads.

[doc]: https://developer.apple.com/documentation/fileprovider/setting-the-finder-sidebar-icon

## Shipping the Platters mark as a custom symbol

To name our own mark there, it has to become a custom SF Symbol: a template SVG
added to the app's asset catalog as a Symbol Image Set. The template is Apple's
file, with our paths substituted into its variant layers — the guide geometry
and the canvas have to stay exactly as exported, and they differ between
template versions, so it is not something to hand-write.

1. In the SF Symbols app, pick any symbol as a base, File > Duplicate as Custom
   Symbol, then File > Export Template. Variable is enough: it contains the
   three interpolation sources (`Ultralight-S`, `Regular-S`, `Black-S`) and the
   system generates the other 24 variants.
2. Draw the mark into it:

   ```
   ./make-symbol.py Exported.svg -o Silo.svg
   ```

3. Validate — SF Symbols' File > Validate Templates, or just drop `Silo.svg`
   into an Xcode asset catalog (Editor > Add New Asset > Symbol Image Set) and
   let Xcode report any error.
4. Name the resulting symbol in `CFBundleSymbolName` as above.

`make-symbol.py` reads the `Capline-*`/`Baseline-*` guides out of the template
you give it and sizes the mark to that cap height, so it does not care which
template version you exported. It fills in every `<g id="Weight-Scale">` the file
contains, always as three four-point paths in the same order — equal path and
point counts across the sources is what makes the template interpolatable, and
what keeps multicolor/hierarchical annotations in sync if we add them later.

The one thing to confirm on the first run is placement: the script draws on a
baseline at local `y = 0`, assuming the variant group's own transform puts the
origin at the baseline and left margin, which is how exported templates are laid
out. If the mark lands off its guides, that assumption is where to look.

## Weights

`silo-symbol-weights.svg` is the ramp the script draws, Ultralight through
Black. Bar thickness is set as the *gap* between bars so three bars and two gaps
always sum to exactly the cap height — the mark keeps its outer box at every
weight and only the slots open and close. Regular is the 12-on-42 proportion the
64px mark is drawn at, so it matches `../silo-mark-*.svg` exactly.

## Until then

`externaldrive.connected.to.line.below` and `rectangle.stack` are the closest
stock symbols and need no asset catalog work, so either is a reasonable
placeholder while the custom symbol is unbuilt.
