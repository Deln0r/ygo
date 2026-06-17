# gomobile binding

App-level mobile SDK for ygo via [`gomobile bind`](https://pkg.go.dev/golang.org/x/mobile/cmd/gomobile),
the official Go cross-compilation toolchain for iOS and Android.

The main `ygo` package exposes a fully idiomatic Go API (channels,
`any`, callbacks, generics) that `gomobile bind` cannot generate
bindings for. This subpackage wraps those types with a bind-safe
surface so an iOS / Android app can edit a document and render UI
while sync runs in the background.

## API levels

- **App level** — editable `Text` (`InsertAt` / `DeleteAt` / `String`,
  UTF-16 indices) and `Map` (string keys/values), `UndoManager`,
  cursor anchors (`Text.EncodeCursor` / `ResolveCursor`), and a sync
  `Client` (`NewClient` / `Connect` / `Listener`) that connects to a
  yserve / Hocuspocus / y-websocket server.
- **Wire level** — bytes-in / bytes-out (`ApplyUpdate`,
  `EncodeStateAsUpdate`, `EncodeDiff`) for adopters bringing their own
  transport.

## Offline-first storage

`Client.EnableOfflineStore(dbPath)` (call before `Connect`) persists the
document to a pure-Go on-device SQLite file. The document loads from it
on connect so the app is usable with no network, every change is saved,
and edits made offline sync up when a connection is next established:

```go
c := gomobile.NewClient("wss://collab.example.com", "note", doc)
c.EnableOfflineStore(appSupportDir + "/note.db")
c.Connect()
```

Pass the app's document path (e.g. `applicationSupportDirectory` on iOS).
No CGO, so the SQLite database needs no native library.

## Observing changes

`Text.ObserveChanges` and `Map.ObserveChanges` deliver the exact change
of each transaction as a **Quill-style delta in JSON** — the shape a
native editor (or Quill / ProseMirror) applies directly, so the UI
updates only what changed instead of re-rendering the whole document:

```go
text := doc.Text("note")
text.ObserveChanges(listener) // listener.OnTextChange([]byte)
// delivers e.g. [{"retain":3},{"insert":"hi","attributes":{"bold":true}}]
```

Implement `TextChangeListener` / `MapChangeListener` in Swift or Kotlin.
Callbacks run on a background goroutine while the document lock is held;
dispatch to the main thread before touching UI.

## Collaborator presence and live cursors

The sync `Client` carries an awareness (presence) channel for ephemeral
per-user state — who is online, their name, their cursor. Publishing
your own and observing everyone else's is the full round trip for
rendering live remote cursors.

**Publish** your state with `SetAwarenessState`, typically a small JSON
object with your name and a cursor encoded from the text you are editing:

```go
cur, _ := doc.Text("note").EncodeCursor(index, 0) // anchor at a position
state, _ := json.Marshal(map[string]any{
    "name":   "ian",
    "cursor": base64.StdEncoding.EncodeToString(cur),
})
client.SetAwarenessState(state)
```

**Observe** everyone else with `Client.ObservePresence`. The listener
receives the whole room as a JSON object keyed by clientID, and fires on
every change — a cursor move, a join, a leave:

```go
client.ObservePresence(listener) // listener.OnPresenceChange([]byte)
// delivers e.g. {"42":{"name":"ian","cursor":"BASE64..."},"77":{"name":"sam"}}
```

For each peer, base64-decode its `cursor` and call `Text.ResolveCursor`
to map it to a local index — the position stays correct even after
concurrent edits shift the text. Skip your own entry with
`Client.ClientID()`; call `Client.PresenceStates()` for the initial
paint before the first change arrives. A peer that disconnects or calls
`RemoveAwarenessState` drops out of the next snapshot.

Implement `PresenceListener` in Swift or Kotlin. Callbacks run on a
background goroutine; dispatch to the main thread before touching UI.

## Verified iOS xcframework build

```bash
# One-time setup
go install golang.org/x/mobile/cmd/gomobile@latest
go install golang.org/x/mobile/cmd/gobind@latest
$(go env GOPATH)/bin/gomobile init

# In a fresh checkout of github.com/Deln0r/ygo:
go get golang.org/x/mobile/bind   # gomobile build dependency
$(go env GOPATH)/bin/gomobile bind -target=ios,iossimulator \
    -o /tmp/Ygo.xcframework \
    github.com/Deln0r/ygo/gomobile
```

Produces a `.xcframework` containing:
- `ios-arm64/Ygo.framework` (~6.6 MB) — real-device slice (arm64)
- `ios-arm64_x86_64-simulator/Ygo.framework` (~13 MB) — simulator slice (arm64 + x86_64, fat)
- Auto-generated Objective-C headers in each slice's `Headers/` dir
  (`Ygo.h`, `Gomobile.objc.h`, `Universe.objc.h`, `ref.h`)

Drag the `.xcframework` into Xcode under "Frameworks, Libraries,
and Embedded Content"; the auto-generated Swift bridging header
exposes `GomobileDoc`, `GomobileAwareness` and helpers like
`GomobileNewDoc`, `GomobileNewDocWithClientID`. Verified on
Xcode 16+, Go 1.26, macOS 26 (Apple Silicon, May 2026).

## Verified Android AAR build

```bash
# One-time: install NDK + at least one SDK platform via sdkmanager.
# (Android Studio's first-launch wizard installs the SDK but not NDK.)
SDK=$HOME/Library/Android/sdk
export JAVA_HOME="/Applications/Android Studio.app/Contents/jbr/Contents/Home"
$SDK/cmdline-tools/latest/bin/sdkmanager --install "ndk;27.0.12077973"
$SDK/cmdline-tools/latest/bin/sdkmanager --install "platforms;android-21"

# Per-build:
export ANDROID_HOME=$SDK
export ANDROID_NDK_HOME=$SDK/ndk/27.0.12077973
go get golang.org/x/mobile/bind   # gomobile build dependency

$(go env GOPATH)/bin/gomobile bind \
    -target=android \
    -androidapi 21 \
    -o /tmp/ygo.aar \
    github.com/Deln0r/ygo/gomobile
```

Produces an `.aar` (Android archive) ~8.4 MB containing native
JNI libraries for all four standard Android architectures:

| Slice | Size |
|---|---|
| `jni/arm64-v8a/libgojni.so` (modern devices) | 3.8 MB |
| `jni/armeabi-v7a/libgojni.so` (older 32-bit) | 3.7 MB |
| `jni/x86_64/libgojni.so` (emulator) | 4.1 MB |
| `jni/x86/libgojni.so` (older emulator) | 3.7 MB |

Plus `classes.jar` exposing the Java surface:
- `gomobile.Doc` — the CRDT document handle (NewDoc / ApplyUpdate
  / EncodeStateAsUpdate / EncodeStateVector / EncodeDiff /
  HasPending / MissingSV)
- `gomobile.Awareness` — the presence layer
- `gomobile.Gomobile` — package-level static helpers
  (`NewDoc()`, `NewDocWithClientID(long)`, `NewAwareness(long)`)
- `go.Seq` + supporting runtime classes

Drop the `.aar` into your Android Studio project's
`app/libs/` directory, add `implementation files('libs/ygo.aar')`
to `build.gradle`, and `import gomobile.Doc;` from Kotlin or
Java. Verified on Android Studio Ladybug + NDK 27.0 + Go 1.26 /
macOS 26 Apple Silicon (May 2026).

**NB on `androidapi 21`**: NDK 27 dropped support for API levels
below 21 (Android 5.0 Lollipop). Without the explicit
`-androidapi 21` flag, gomobile defaults to 16 and fails with
"unsupported API version". 21+ covers >99% of Android devices
in service.

## Note on `go.mod`

`gomobile bind` requires `golang.org/x/mobile/bind` to be present
in the module's dependency graph at build time. The main `go.mod`
does NOT carry this dep (it would bump the `go` directive past
1.22 and break our CI Go-version matrix). Adopters running their
own `gomobile bind` should `go get golang.org/x/mobile/bind` in
their fresh checkout before the bind step (see commands above);
the dep is build-time only, no runtime cost.
