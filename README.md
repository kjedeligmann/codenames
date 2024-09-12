# codenames
## A simple multiplayer game running on WebSockets

Server side is written in Go, using standard library for the most part (`net/http`, `html/template` etc). Gameplay is implemented using `gorilla/websocket` package.

Frontend is in HTMX, so it is thin but relies heavily on the server, where user input is validated, rendered and an HTML response is sent back. 

## Setup

To try it out locally, you can clone the repo, then build the app and run it:

```
git clone https://github.com/kjedeligmann/codenames.git
cd codenames
go build
./codenames
```

You should be able to access it on `localhost:3000` now. Go to `/` to create a game with your desired wordlist, then grab the `<game-id>` and switch to `/game/<game-id>` to join the game. The others can join or watch the game via the same link.
