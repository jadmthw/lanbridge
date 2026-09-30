# LANBridge

Play Minecraft: Java Edition **Open to LAN** worlds with friends over the internet, as if you were all on the same Wi-Fi. No server to rent, no mods, no port forwarding in most cases.

Works with any Java Edition version that has Open to LAN, including 26.3 and newer. LANBridge never looks inside game traffic, so new Minecraft versions don't break it.

| Platform | Download |
| --- | --- |
| Windows on ARM (Snapdragon) | `LANBridge-windows-arm64.exe` |
| Windows x64 | `LANBridge-windows-x64.exe` |
| macOS, Apple silicon | `lanbridge-macos-arm64.tar.gz` |
| macOS, Intel | `lanbridge-macos-x64.tar.gz` |
| Linux x64 / ARM64 | `lanbridge-linux-x64.tar.gz` / `lanbridge-linux-arm64.tar.gz` |

Get them from the [latest release](../../releases/latest). On macOS and Linux you can also install from a terminal:

```sh
curl -fsSL https://github.com/OWNER/lanbridge/releases/latest/download/install.sh | sh
```

(Replace `OWNER` with the GitHub account that hosts this repo. The copy of `install.sh` attached to each release already has it filled in.)

## Hosting a world

1. Open your world in Minecraft, press **Esc → Open to LAN → Start LAN World**.
2. Start LANBridge (double-click it, or run `lanbridge`). A control panel opens in your browser.
3. Click **Start hosting**. LANBridge finds your world by itself, then shows an **invite code**.
4. Send the code to your friends. Keep LANBridge open while you play.

The code stays the same between sessions, so friends can reuse it. **Make a new code** cuts off everyone who has the old one.

## Joining a friend

1. Start LANBridge, pick **Join a friend** and paste the code.
2. In Minecraft go to **Multiplayer**. The world shows up at the bottom of the list like a LAN game. You can also use **Direct Connection** with the address LANBridge shows (usually `localhost`).

Friends only need LANBridge and the code, never port forwarding or a relay of their own.

## How friends reach you

LANBridge tries every route at once and uses the first one that works:

- **Same network**: always works.
- **Direct over the internet**: LANBridge asks your router to open a port (UPnP), then has the relay double-check that it's reachable. Also works over IPv6, or if you forwarded port 42525 yourself (tick the box in Settings).
- **Relay**: for everything else, like dorms, school and work networks, phone hotspots, or internet providers that share one address between customers (CGNAT). The host keeps a connection open to a relay and friends meet it there.

The control panel shows which routes are working for you.

## AI players (new in 2.0)

Bring GPT, Claude, Grok or Gemini into your world as players. They show up like anyone else, everybody in the world can talk to them in chat, and they can follow you, walk places, mine, fight mobs and hand you items.

**What the host needs:** Minecraft: Java Edition 26.3 or newer with [Fabric](https://fabricmc.net/use/installer/) and the [Carpet mod](https://modrinth.com/mod/carpet), on the computer that hosts the world. Friends who join don't need any mods. Vanilla LAN worlds only let in players with their own Microsoft account, so AI players are Carpet "fake players" instead.

**Setup (once):**

1. In LANBridge, open **AI players** (top right).
2. Click **Install script** next to your Minecraft folder.
3. Set up at least one model (see below) and click **Test**.
4. Open your world with commands allowed (**Open to LAN → Allow Commands: ON**) and type `/script load lanbridge` in chat. Do this once per play session.

**Adding and talking to them:** use **Add to world** in LANBridge, or type in Minecraft chat:

```text
!ai spawn claude              adds Claude_1 next to you
!ai spawn gpt Steve a grumpy dwarf who loves mining
!ai remove Steve              (or: !ai remove all)
!ai stop all                  stop whatever they're doing
!ai list
```

Talk to one by saying its name ("Claude_1, get me some wood"). Once you're talking with one, follow-ups go to it automatically for a minute or so. Chat that doesn't name anyone gets answered by the closest AI player (turn that off under **Permissions and limits**). AI players never answer each other, so they can't get stuck talking in circles.

**Commands and building:** AI players can run Minecraft commands ("Claude_1, make it day and clear the weather") and build ("GPT_1, build me a small stone house with a door"). Builds go on the ground a few blocks in front of whoever asked. The AI player first surveys the terrain (heights, water, trees, existing buildings), then a design step plans the whole build with proper foundations, roofs and details, places it with fill and setblock, and fixes any steps the game rejects. Big builds take a minute or two on slower models, and the AI players page shows the progress. By default only the host can ask for commands or builds; you can allow everyone or no one under **Permissions and limits**. Commands run with operator powers, but anything that could take over or break the world (op, deop, stop, ban, kick, whitelist, reload, script, carpet, function and similar) is always blocked, including when it's hidden inside /execute.

By default only the host can add or remove AI players from chat; you can change that, and the per-minute reply limit, in the same place.

**Paying for the models:**

| Model | On your subscription | With an API key |
| --- | --- | --- |
| GPT | ChatGPT Plus, Pro or Business, through OpenAI's [Codex CLI](https://github.com/openai/codex) (`npm i -g @openai/codex`, then `codex login`) | OpenAI API credits |
| Grok | SuperGrok or X Premium+, through xAI's [Grok Build CLI](https://x.ai/cli) (`grok login`) | xAI API credits |
| Claude | Not possible: Anthropic doesn't allow third-party apps to use Claude Free, Pro or Max plans | Anthropic Console credits |
| Gemini | Not possible: Google ended Gemini CLI sign-in for personal accounts | Google AI Studio key (has a free tier) |

API keys are saved only on your computer, in LANBridge's settings file. When an AI player replies, the message, recent chat and a short description of its surroundings go to that model's provider.

**Current limits:** AI players steer toward targets in a straight line (hopping over blocks) rather than pathfinding, so they teleport to catch up if they get stuck or fall far behind. They build with commands, not block by block, and can't craft yet. The in-game script is `lanbridge.sc`; it only ever controls Carpet fake players, never real ones.

## Relays

A relay is tiny and can run for free. Only the host needs to know about it, since its address is inside the invite code. Releases can also come with a relay built in (see *Releasing*).

**Any Linux server** (for example an Oracle Cloud Always Free VM or any cheap VPS):

```sh
curl -fsSL https://github.com/OWNER/lanbridge/releases/latest/download/install-relay.sh | sudo sh
```

It installs a service on port 7777 and prints the **relay address** and **token** to paste into LANBridge's Settings. Allow TCP 7777 in your cloud provider's firewall or security list.

**Docker**:

```sh
docker build -t lanbridge-relay .
docker run -d --restart=always -p 7777:7777 -e LANBRIDGE_RELAY_TOKEN=pick-a-secret lanbridge-relay
```

**Render** (free web service): create a Blueprint from this repo. The relay address is `wss://<service-name>.onrender.com`, and the generated token is in the service's Environment tab. Free services may go to sleep when unused, so the first connection can take a minute.

A relay can't read or change your game: traffic is encrypted end to end between each friend and the host. The token only stops strangers from hosting on your relay.

## Command line

```text
lanbridge                     open the control panel
lanbridge host [flags]        share your Open to LAN world from the terminal
lanbridge join <code>         join a friend's world from the terminal
lanbridge check <code>        test whether a host can be reached
lanbridge relay [flags]       run a relay (listens on $PORT or :7777)
lanbridge selftest            check that everything works on this computer
(AI players are managed from the control panel and in-game chat.)
lanbridge version
```

Useful flags: `host --mc-port 54321 --relay tcp://1.2.3.4:7777 --token … --no-upnp --forwarded`, `join --local-port 25566 --share-lan`, `relay --listen :7777 --token … --trust-proxy`. Run `lanbridge <command> -h` for the full list. The relay also reads `LANBRIDGE_RELAY_TOKEN` and `LANBRIDGE_TRUST_PROXY=1`.

## Troubleshooting

- **Windows says "Windows protected your PC"**: click **More info → Run anyway**. The app isn't code-signed yet.
- **Windows Firewall asks about LANBridge**: allow it. Otherwise LAN detection and direct connections can't work.
- **macOS says the app can't be opened**: use the install command above, or run `xattr -d com.apple.quarantine lanbridge` once. If macOS asks about finding devices on your local network, allow it; that's how LANBridge sees your Open to LAN world.
- **"Waiting for a world"**: make sure you clicked **Start LAN World**. If it's still not found, type in the port Minecraft printed in chat ("Local game hosted on port …").
- **Friends can't connect and the relay route says "No relay set up"**: your network blocks incoming connections. Set up a relay (above).
- **The world doesn't show in the Multiplayer list**: use Direct Connection with the address LANBridge shows.
- **Old invite code**: friends see "the host didn't accept this invite code". Send them the current one.

Settings live in `%AppData%\LANBridge` on Windows, `~/Library/Application Support/LANBridge` on macOS and `~/.config/LANBridge` on Linux.

## How it works

Minecraft announces LAN worlds by UDP multicast (`224.0.2.60:4445`). The host side picks up that announcement to learn the world's port. The friend's side opens a local port and announces a stand-in world to its own Minecraft, so the world appears in the Multiplayer list. Each Minecraft connection is carried over TLS 1.3, and both ends prove they hold the invite's secret by an HMAC over the TLS session's exported keying material. Anyone in the middle, relay included, can't read the traffic, change it or impersonate either side.

Written in Go with only the standard library. Builds are fully static, with no runtime or installer.

## Building and releasing

```sh
go test ./...
./scripts/build.sh            # all platforms into dist/
```

Every push to `main` runs the tests, builds all platforms, runs each binary's self-test on Windows, macOS and Linux, and publishes a GitHub release named after `VERSION` if that release doesn't exist yet. To ship an update, bump `VERSION` and push.

To build a relay into release binaries so nobody has to configure one, set the repository variable `LANBRIDGE_DEFAULT_RELAY` (Settings → Secrets and variables → Actions → Variables) to your relay address, like `wss://lanbridge-relay.onrender.com`. Leave the relay token empty for a relay you want everyone to use.

## License

MIT. LANBridge is not affiliated with or endorsed by Mojang or Microsoft. Minecraft is a trademark of Mojang Synergies AB.
