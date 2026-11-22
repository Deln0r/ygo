module github.com/Deln0r/ygo/integration/matrix

go 1.26.0

require (
	// v1.18.0 is the first core release containing ygo.ValidateUpdate, which
	// this module calls. The pin and the core tag are cut from the same
	// commit deliberately: a `replace` in a dependency's go.mod is ignored by
	// consumers, so this line is what an adopter actually resolves, and
	// pinning anything older publishes a module that cannot compile
	// (measured: `undefined: ygo.ValidateUpdate`).
	github.com/Deln0r/ygo v1.30.0
	maunium.net/go/mautrix v0.33.0
)

require (
	filippo.io/edwards25519 v1.2.0 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/rs/zerolog v1.35.1 // indirect
	github.com/tidwall/gjson v1.20.0 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.3.0 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
	go.mau.fi/util v0.11.1 // indirect
	golang.org/x/crypto v0.59.0 // indirect
	golang.org/x/exp v0.0.0-20261113113407-e3970619a1a4 // indirect
	golang.org/x/net v0.62.0 // indirect
	golang.org/x/sys v0.50.0 // indirect
	golang.org/x/text v0.44.0 // indirect
)

replace github.com/Deln0r/ygo => ../..
