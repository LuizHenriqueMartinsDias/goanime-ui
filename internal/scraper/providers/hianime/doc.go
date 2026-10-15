// Package hianime is the leaf scraper for hianime.at.
//
// It replaces the anidb.app path, which died the same way the AllAnime path
// before it did. On 2026-09-22 anidb.app answered 503 with an "Under
// Maintenance" body on EVERY path — the homepage, /browse and the
// /api/frontend/... endpoints alike — so nothing on that host is parseable at
// all. Upstream ani-cli had already moved to hianime.at; this package follows,
// written against the site's own traffic rather than ported from anything.
//
// The chain is four plain requests. No Cloudflare challenge, no rotating token
// and no per-request key derivation of the kind that killed AllAnime:
//
//	GET /search?keyword=<query>                   → anime cards (HTML)
//	GET /api/theme/episode/list/<animeID>         → {"status","totalItems","html"}
//	GET /api/theme/episode/servers?episodeId=<id> → {"status","html"}
//	GET <embed URL>                               → the player payload
//
// The two JSON endpoints answer with an HTML fragment inside a JSON envelope,
// which is why both a decoder and goquery appear in the same call.
//
// A server entry carries its embed URL as data-hash, which is plain base64.
// ZokoAnime is read first: its page ships the whole player config in one
// obfuscated blob (see embed.go), and it was present on every title sampled,
// for every audio track. Its CDN is a single host that some ISPs drop (seen
// 2026-10-15 from a Brazilian connection: no TCP answer while the rest of the
// world reached it), so stream.go probes the playlist and falls back to the
// MegaPlay family ("HD-1", …). That one hands back an AES-encrypted "enc" field
// whose key the player's own script carries, and segments wrapped in a PNG
// image that a local proxy unwraps (see megaplay.go and hlsproxy.go).
//
// Identity is carried in URLs so the source registry can route an anime back
// here without extra state:
//
//	anime:   https://hianime.at/<slug>-<animeID>
//	episode: https://hianime.at/watch/<slug>-<animeID>?ep=<episodeID>
//
// Audio: "sub" is subtitled and "dub" is dubbed; some titles also carry "hsub".
// Subtitled is preferred and the rest are fallbacks; GOANIME_HIANIME_AUDIO
// overrides the preference.
package hianime
