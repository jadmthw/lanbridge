2.2.3: every AI build now has a live build log (on the AI players page and in Activity) showing each stage with timings, the model's own progress and how many tokens the design used. Stopping or timing out a design now really ends the Codex or Grok process on Windows instead of leaving it running. Block placement gives the game more time on heavy fills and never re-places steps that were only slow.

2.2.2: AI players know when they're in the middle of a build, so they answer "what are you doing?" honestly instead of starting it over, and saying "stop" (or clicking Stop task) now cancels a build.

2.2.1 fixes and improves AI building.

- AI players now know redstone: repeaters, comparators, observers, pistons, lamps, clocks and lamp displays, with the correct block directions. Builds can also place signs and decoration entities like text displays.
- Designs that come back empty now say so, instead of just "The build didn't work".
- Removing an AI player while it's designing no longer shows a fake "timed out" error from a player who has left.
- AI player names can't be slurs.

2.2.0 makes AI builds much better.

- Builds now go through a separate design step. The AI player surveys the ground around the build spot (heights, water, trees and anything already built), and the model gets a full block and block-state reference plus real building technique: foundations that follow the terrain, framed walls, proper stair roofs with overhangs, lighting and interiors.
- Builds can be much bigger (up to 800 commands instead of about 150), and slow thinking models like GPT-6 Astra get up to 12 minutes instead of timing out after 2.
- If the game rejects part of a build (a typo in a block name, for example), the model gets the errors back and fixes them.
- The AI player keeps chatting while it builds, and the AI players page shows what stage it's at.
- Reinstall the script from the AI players page and run /script load lanbridge again (it has a new terrain survey).

2.1.1: if your world is still running an older copy of the in-game script, the AI players page now says so and tells you how to update it, instead of "unknown op run".

2.1.0 lets AI players run commands and build.

- Ask an AI player to run Minecraft commands ("make it night", "give me 10 torches") or to build ("build a little cabin with a chimney"). Builds go right in front of you.
- AI players now answer chat even when you don't use their name (the closest one replies), and they never answer each other.
- New Permissions and limits: choose who can ask for commands and builds (nobody, just the host, or everyone). Commands that could take over or break the world, like op, stop, ban and script, are always blocked.
- After updating, reinstall the script from the AI players page and run /script load lanbridge again.

2.0.3: fixes AI players never spawning. The in-game script misread the names of LANBridge's command files, so commands were never picked up. Reinstall the script from the AI players page and run /script load lanbridge again.

2.0.2: LANBridge now notices when single-player Minecraft is paused (it pauses whenever you switch windows) and says so, instead of "Minecraft didn't answer". AI players also survive a pause instead of being dropped.

2.0.1 fixes two display glitches on the AI players page (a stray "null" under GPT, and step numbers on the Minecraft folder list) and clarifies the model box when using a ChatGPT or SuperGrok plan.

LANBridge 2.0 adds AI players.

- Bring GPT, Claude, Grok or Gemini into your world as players, and talk to them in normal Minecraft chat
- They can follow you, come to you, walk to coordinates, mine blocks, fight mobs, hand you items, switch tools and eat
- Add and remove as many as you like from the new AI players page, or in game with `!ai spawn claude`, `!ai remove`, `!ai list`
- GPT can run on your ChatGPT plan (through OpenAI's Codex CLI) and Grok on your SuperGrok plan (through xAI's Grok Build CLI). Claude and Gemini use API keys, because Anthropic and Google don't allow third-party apps to use their consumer plans. Gemini's AI Studio keys have a free tier.
- Limits for replies per minute and number of AI players keep your credits in check
- Needs Fabric and the Carpet mod on the host's Minecraft (26.3 or newer). Friends don't need any mods.

Everything from 1.0 is unchanged: share an Open to LAN world with a copy-paste invite code, direct connections when possible and an end-to-end encrypted relay otherwise.

Downloads are below. On Windows, click "More info → Run anyway" if SmartScreen warns you. On macOS or Linux, you can install with:

    curl -fsSL https://github.com/__REPO__/releases/latest/download/install.sh | sh
