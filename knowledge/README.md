# knowledge

The workspace's own knowledge packs, in the shape a Gator registry expects: `index.json`
listing each pack, and a folder per pack holding `pack.json` and its Markdown.

A registry can be any git repository with this layout; this one is embedded in gator so a
fresh install has something real to enable. Compute a pack's checksum with
`gator-server knowledge checksum <pack dir>` and paste it into `index.json`; a pack whose
files do not hash to the value in the index is refused.
