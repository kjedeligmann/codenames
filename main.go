package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	Blue  = "blue"
	Red   = "red"
	White = "white"
	Black = "black"
)

const (
	Operative = "o"
	Spymaster = "s"
)

const Size = 5

type Cell struct {
	Word   string
	Color  string
	IsOpen bool
}

type Board [Size][Size]Cell

func lineCounter(r io.Reader) (int, error) {
	buf := make([]byte, 32*1024)
	count := 0
	lineSep := []byte{'\n'}

	for {
		c, err := r.Read(buf)
		count += bytes.Count(buf[:c], lineSep)

		switch {
		case err == io.EOF:
			return count, nil

		case err != nil:
			return count, err
		}
	}
}

func Words(name string) (words [25]string, e error) {
	wordlist, err := os.Open(fmt.Sprintf("wordlists/%s.txt", name))
	if err != nil {
		return words, err
	}
	defer wordlist.Close()

	length, err := lineCounter(wordlist)
	if err != nil {
		return words, err
	}
	// debug
	log.Println(name)
	log.Println(length)

	var wordIdcs [25]int
	present := map[int]struct{}{}
	for i := range wordIdcs {
		for {
			idx := rand.Intn(length)
			if _, ok := present[idx]; ok {
				continue
			}
			present[idx] = struct{}{}
			wordIdcs[i] = idx
			break
		}
	}
	slices.Sort(wordIdcs[:])
	log.Println(wordIdcs)

	// reopen the file to scan it again
	again, err := os.Open(fmt.Sprintf("wordlists/%s.txt", name))
	if err != nil {
		return words, err
	}
	defer again.Close()

	scanner := bufio.NewScanner(again)
	curr := 0
	for i := range wordIdcs[len(wordIdcs)-1] + 1 {
		log.Println(i)
		if scanner.Scan() == false {
			return words, scanner.Err()
		}
		if i == wordIdcs[curr] {
			words[curr] = scanner.Text()
			log.Println(curr, words[curr])
			curr++
		}
	}
	log.Println(words)

	rand.Shuffle(25, func(i, j int) {
		words[i], words[j] = words[j], words[i]
	})

	log.Println(words)

	return words, nil
}

func NewBoard(wordlist string) *Board {
	// shuffle word colors
	var colors = []string{
		Blue, Blue, Blue, Blue, Blue, Blue, Blue, Blue, Blue,
		Red, Red, Red, Red, Red, Red, Red, Red,
		White, White, White, White, White, White, White,
		Black,
	}
	rand.Shuffle(25, func(i, j int) {
		colors[i], colors[j] = colors[j], colors[i]
	})

	words, err := Words(wordlist)
	if err != nil {
		log.Println(err)
		return &Board{}
	}

	var b Board
	var idx int
	for i := 0; i < Size; i++ {
		for j := 0; j < Size; j++ {
			b[i][j].Word = words[idx]
			b[i][j].Color = colors[idx]
			idx++
		}
	}
	return &b
}

type Player struct {
	ID       string
	Nickname string
	Team     string
	Role     string
	conn     *websocket.Conn
}

type Team struct {
	Operative *Player
	Spymaster *Player
	WordsLeft int
}

// type Turn struct {
// 	Team string
// 	Role string
// }

type Game struct {
	ID     string
	Board  *Board
	Red    Team
	Blue   Team
	Winner *Team
	Turn   *Team
	Clue   *Clue
	Begun  bool
	ended  bool
	moves  chan []byte
	lobby  map[*websocket.Conn]struct{}
}

type JoinRequest struct {
	// PlayerId string
	GameID string `json:"gameID"` // read the gameID from the HX-Current-URL header?
	Team   string
	Role   string
}

const JoinBroadcast = `
<div id="{{.Team}}{{.Role}}">
    {{Role .Role}}: {{.Nickname}}
</div>
`

var JoinFuncMap = template.FuncMap{
	"Role": func(role string) string {
		if role == "o" {
			return "Operative"
		} else if role == "s" {
			return "Spymaster"
		} else {
			return "Invalid role"
		}
	},
	// for passing multiple arguments to a template
	"map": MapTempl,

	"safe": func(s string) template.CSS {
		return template.CSS(s)
	},
}

func MapTempl(pairs ...any) (map[string]any, error) {
	if len(pairs)%2 != 0 {
		return nil, fmt.Errorf("misaligned map")
	}

	m := make(map[string]any, len(pairs)/2)

	for i := 0; i < len(pairs); i += 2 {
		key, ok := pairs[i].(string)

		if !ok {
			return nil, fmt.Errorf("cannot use type %T as map key", pairs[i])
		}
		m[key] = pairs[i+1]
	}
	return m, nil
}

const OwnID = `
<div id="player-id" hidden>{{.ID}}</div>
`
const EnterNickname = `
<div id="{{.Team}}{{.Role}}" hx-ext="ws">
        <input id="nickname" type="text" placeholder="Nickname">
        <button ws-send
                hx-vals='js:{
                "playerID": document.getElementById("player-id").textContent,
                "gameID": window.location.href.split("/")[4],
                "nickname": document.getElementById("nickname").value,
                }'
                hx-trigger="click"
                hx-swap="outerHTML"
                >Submit</button>
</div>
`

const SomeoneHasJoined = `
<div id="{{.Team}}{{.Role}}">
    Someone has joined...
</div>
`

const WordlistsView = `
<select name="wordlist" id="wordlist">
    {{ range $i, $name := . }}
        <option value="{{$name}}">{{$name}}</option>
    {{ end }}
</select>
`

type Clue struct {
	PlayerID string
	GameID   string
	Team     string
	Word     string
	Number   int
}

type Guess struct {
	PlayerID string
	GameID   string
	//Action string
	Col         int
	Row         int
	EndGuessing bool
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

var players = map[string]*Player{}
var games = map[string]*Game{}
var pLock = sync.RWMutex{}
var gLock = sync.RWMutex{}

func main() {
	log.Println("codenames server started")
	mux := http.NewServeMux()

	// adding a file server for local htmx lib and ws ext
	mux.Handle("/htmx/", http.FileServer(http.Dir(".")))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// send the html with create button with hx-post to /create
		// create the list of wordlists on fly by request to /wl
		http.ServeFile(w, r, "create.html")
	})

	mux.HandleFunc("GET /wl", func(w http.ResponseWriter, r *http.Request) {
		list, err := os.ReadDir("wordlists")
		if err != nil {
			log.Println(list, err)
			return
		}
		log.Print(list, err)

		// execute the template and send it back
		var names []string
		for _, file := range list {
			name := file.Name()
			if strings.HasSuffix(name, ".txt") {
				names = append(names, strings.TrimSuffix(name, ".txt"))
			}
		}
		if err := template.Must(template.New("wl").Parse(WordlistsView)).Execute(w, names); err != nil {
			log.Println(err)
			return
		}
	})

	mux.HandleFunc("GET /game/{id}", func(w http.ResponseWriter, r *http.Request) {
		gameId := r.PathValue("id")
		log.Printf("get /game/%s", gameId)

		if game, ok := games[gameId]; ok {
			gamePage := template.Must(template.New("game").
				Funcs(JoinFuncMap).
				ParseFiles("game.html", "teams.html", "board.html", "clue.html"))

			if err := gamePage.ExecuteTemplate(w, "game.html", game); err != nil {
				log.Println(err)
				return
			}
		} else {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
	})

	mux.HandleFunc("POST /create", func(w http.ResponseWriter, r *http.Request) {
		log.Println("post /create")
		wordlist := r.FormValue("wordlist")
		if _, err := os.Stat(fmt.Sprintf("wordlists/%s.txt", wordlist)); errors.Is(err, os.ErrNotExist) {
			// file does not exist
			// send the client an error somehow?
			return
		}
		newGame := &Game{
			ID:    uuid.New().String(),
			Board: NewBoard(wordlist),
			Blue: Team{
				WordsLeft: 9,
			},
			Red: Team{
				WordsLeft: 8,
			},
			Winner: nil,
			Turn:   nil,
			Clue:   nil,
			Begun:  false,
			moves:  make(chan []byte),
			lobby:  map[*websocket.Conn]struct{}{},
		}

		// adding newGame to games map
		gLock.Lock()
		games[newGame.ID] = newGame
		gLock.Unlock()

		// sending the gameId back to the client
		resp := []byte(newGame.ID)

		// I should change it to this somehow
		// resp := []byte("<a href=\"http://my.domain/codenames/game/" + newGame.ID + "\">http://my.domain/codenames/game/" + newGame.ID + "</a>")
		// but with domain specified elsewhere
		// maybe inside a flag?

		if _, err := w.Write(resp); err != nil {
			log.Println(err)
			return
		}

		// for testing purposes
		log.Println("new game ID", newGame.ID)
		log.Println("map entry", games[newGame.ID])
	})

	mux.HandleFunc("/join", func(w http.ResponseWriter, r *http.Request) {
		log.Println("/join")
		// upgrading the connection to the WebSocket protocol
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Println(err)
			return
		}

		// websocket should immediately send the gameID for lobby to the server
		_, lobbyGameID, err := conn.ReadMessage()
		if err != nil {
			log.Println(err)
			return
		}
		lgid := struct {
			GameID string `json:"gameID"`
		}{}
		if err = json.Unmarshal(lobbyGameID, &lgid); err != nil {
			log.Println(err)
			return
		}
		log.Println(lgid)

		// checking if the sought game exists
		gLock.RLock()
		_, ok := games[lgid.GameID]
		gLock.RUnlock()
		if !ok {
			log.Printf("No game with ID %s exists", lgid.GameID)
			return
		}

		// adding the connection to the lobby for it to be reached somehow even though player has not joined the game
		// maybe I should reconsider using websockets for this and try SSE instead
		games[lgid.GameID].lobby[conn] = struct{}{}

		// reading client request to join a particular game with a particular role
		_, joinData, err := conn.ReadMessage()
		if err != nil {
			log.Println(err)
			return
		}
		join := JoinRequest{}
		if err = json.Unmarshal(joinData, &join); err != nil {
			log.Println(err)
			return
		}
		log.Println(join)

		// checking if the sought game exists
		gLock.RLock()
		game, ok := games[join.GameID]
		gLock.RUnlock()
		if !ok {
			log.Printf("No game with ID %s exists", join.GameID)
			return
		}
		// for testing purposes
		log.Println("join gameID", join.GameID)
		log.Println("game struct", game)

		// creating a new player with unique ID
		newPlayer := Player{
			ID:   uuid.New().String(),
			Team: join.Team,
			Role: join.Role,
			conn: conn,
		}
		// for testing purposes
		log.Println("new player id", newPlayer.ID)
		log.Println("new player struct", newPlayer)

		// checking if the role is already occupied
		// adding the newPlayer to the game and players map if it isn't
		// pLock.Lock()
		if join.Team == Blue {
			if join.Role == Operative {
				if game.Blue.Operative == nil {
					players[newPlayer.ID] = &newPlayer
					game.Blue.Operative = &newPlayer
					log.Println("blue o", game.Blue.Operative)
				} else {
					log.Println("blue o is already there")
					return
				}
			} else if join.Role == Spymaster {
				if game.Blue.Spymaster == nil {
					players[newPlayer.ID] = &newPlayer
					game.Blue.Spymaster = &newPlayer
					log.Println("blue s", game.Blue.Spymaster)
				} else {
					log.Println("blue s is already there")
					return
				}
			}
		} else if join.Team == Red {
			if join.Role == Operative {
				if game.Red.Operative == nil {
					players[newPlayer.ID] = &newPlayer
					game.Red.Operative = &newPlayer
					log.Println("red o", game.Red.Operative)
				} else {
					log.Println("red o is already there")
					return
				}
			} else if join.Role == Spymaster {
				if game.Red.Spymaster == nil {
					players[newPlayer.ID] = &newPlayer
					game.Red.Spymaster = &newPlayer
					log.Println("red s", game.Red.Spymaster)
				} else {
					log.Println("red s is already there")
					return
				}
			}
		}
		// pLock.Unlock()

		// delete player from the lobby after they joined - to distinct between players and observers during the game
		delete(game.lobby, conn)

		// sending the player his ID to place in a player-id div
		ownID := template.Must(template.New("ownID").Parse(OwnID))
		var respOwnID bytes.Buffer
		if err := ownID.Execute(&respOwnID, newPlayer); err != nil {
			log.Println(err)
			return
		}
		if err = conn.WriteMessage(websocket.TextMessage, respOwnID.Bytes()); err != nil { // binary instead of text message was the cause of why it didn't swap the content
			log.Println(err)
			return
		}

		// sending the player the input for his nickname
		var enterNickname bytes.Buffer
		if err := template.Must(template.New("enter-nickname").
			Parse(EnterNickname)).
			Execute(&enterNickname, newPlayer); err != nil {
			log.Println(err)
			return
		}
		if err = conn.WriteMessage(websocket.TextMessage, enterNickname.Bytes()); err != nil {
			log.Println(err)
			return
		}

		// sending the players 'someone has joined' div (to everyone except the joined player)
		var someoneHasJoined bytes.Buffer
		if err := template.Must(template.New("someonejoined").
			Parse(SomeoneHasJoined)).
			Execute(&someoneHasJoined, newPlayer); err != nil {
			log.Println(err)
			return
		}

		playersHere := []*Player{
			game.Blue.Operative,
			game.Blue.Spymaster,
			game.Red.Operative,
			game.Red.Spymaster,
		}

		for _, player := range playersHere {
			if player == nil {
				continue
			}
			// everyone except the one who joins!
			if player.conn == conn {
				continue
			}
			if err = player.conn.WriteMessage(websocket.TextMessage, someoneHasJoined.Bytes()); err != nil {
				log.Println(err)
				return
			}
		}
		for connection := range game.lobby {
			if connection == nil {
				continue
			}
			if err = connection.WriteMessage(websocket.TextMessage, someoneHasJoined.Bytes()); err != nil {
				log.Println(err)
				delete(game.lobby, connection)
				return
			}
		}
		// receiving the players nickname
		_, nicknameData, err := conn.ReadMessage()
		if err != nil {
			log.Println(err)
			return
		}
		nn := struct {
			PlayerID string
			GameID   string `json:"gameID"`
			Nickname string
		}{}
		if err = json.Unmarshal(nicknameData, &nn); err != nil {
			log.Println(err)
			return
		}
		log.Println(nn)

		// setting tha nickname
		newPlayer.Nickname = nn.Nickname

		// sending the connected players the new player div instead of a button
		newPlayerDiv := template.Must(template.New("joinBrdcst").
			Funcs(JoinFuncMap).
			Parse(JoinBroadcast))
		var resp bytes.Buffer
		if err := newPlayerDiv.Execute(&resp, newPlayer); err != nil {
			log.Println(err)
			return
		}

		// sending the players tha div with tha nickname
		for _, player := range playersHere {
			if player == nil {
				continue
			}
			if err = player.conn.WriteMessage(websocket.TextMessage, resp.Bytes()); err != nil { // binary instead of text message was the cause of why it didn't swap the content
				log.Println(err)
				return
			}
		}

		// sending the same thing for the lobby
		// it's just repeating the other way
		// but this allows spectators potentially the same capabilities that the players, maybe I should indeed switch to SSE
		for connection := range game.lobby {
			if connection == nil {
				continue
			}
			if err = connection.WriteMessage(websocket.TextMessage, resp.Bytes()); err != nil {
				log.Println(err)
				delete(game.lobby, connection)
				return
			}
		}

		// explore the game itself after setting up proper client updates
		if game.Blue.Operative != nil &&
			game.Blue.Spymaster != nil &&
			game.Red.Operative != nil &&
			game.Red.Spymaster != nil &&
			game.Begun == false {

			// a bit vice a versa, but gets the point across
			game.Begun = true
			go game.Begin()
		}
	})

	log.Fatal(http.ListenAndServe(":3000", mux))
}

const EndGuessing = `
<span id="end-guessing">
        <button ws-send
                hx-vals='js:{
                "playerID": document.getElementById("player-id").textContent,
                "gameID": window.location.href.split("/")[4],
                "endGuessing": true,
                }'
                hx-trigger="click"
                hx-swap="outerHTML"
                >End Guessing</button>
</span>
`

// adding white textcolor for open black cell is not necessary as the board template with this coloring gets sent to everyone after the winner is decided
// or should it be the other way around?
const OpenCell = `
<button class="cell" id="cell{{.Col}}-{{.Row}}" style="background-color:{{ .Color }};">
    {{ .Word }}
</button>
`

func (game *Game) Begin() {
	// setting blue spymaster as the first player to act
	game.Turn = &game.Blue
	next := &game.Red

	for !game.ended {
		game.sendBoardToEveryone()
		curr := game.Turn

		// move logic

		// spymaster part
		// --------------
		// you should send the spymaster his clue form    execute the ClueForm template
		clueFormTempl := template.Must(template.New("clue-form").ParseFiles("clue.html"))
		var clueForm bytes.Buffer
		if err := clueFormTempl.Execute(&clueForm, nil); err != nil {
			log.Println(err)
			return
		}
		if err := curr.Spymaster.conn.WriteMessage(websocket.TextMessage, clueForm.Bytes()); err != nil {
			log.Println(err)
			return
		}

		// spymaster begins by giving a clue
		_, move, err := curr.Spymaster.conn.ReadMessage()
		if err != nil {
			log.Println(err)
			continue
		}

		var clue *Clue
		if err := json.Unmarshal(move, &clue); err != nil {
			log.Println(err)
			return
		}
		// for debugging purposes
		log.Println(clue)

		// some clue validation should occur
		pLock.RLock()
		if player, ok := players[clue.PlayerID]; !ok || player.ID != curr.Spymaster.ID || player != curr.Spymaster {
			log.Println("Invalid player")
			pLock.RUnlock()
			continue
		}
		pLock.RUnlock()

		// if it's valid, set it as game.Clue and send it to everyone
		clue.Team = players[clue.PlayerID].Team
		game.Clue = clue
		game.giveClue()

		// operative part
		// --------------
		// then comes the operative that sees the clue and clicks the words
		// for the span of clues number + 1 we wait for his messages, or he sends endguessing and we break

		// somehow I need to think of the way that other players do not disrupt the game
		// also I think players should be able to select possible words while clicking the button the first time, and everyone should see this (for example, by making its textcolor yellow or something)

		endGuessingTempl := template.Must(template.New("end-guessing").Parse(EndGuessing))
		var endGuessing bytes.Buffer
		if err := endGuessingTempl.Execute(&endGuessing, nil); err != nil {
			log.Println(err)
			return
		}
		// send an endguessing button to operative and somehow permit him to press the words (while others cannot)
		if err := curr.Operative.conn.WriteMessage(websocket.TextMessage, endGuessing.Bytes()); err != nil {
			log.Println(err)
			return
		}

		// creating a board template for operative
		boardTmpl := template.Must(template.New("board").
			Funcs(template.FuncMap{
				"map": MapTempl,
				"safe": func(s string) template.CSS {
					return template.CSS(s)
				},
			}).
			ParseFiles("board.html"))

		// render a clicky board for current operative and send it to him
		var clickyBoard bytes.Buffer
		if err := boardTmpl.Execute(&clickyBoard, struct {
			Role  string
			Board *Board
			Turn  bool
		}{Operative, game.Board, true /* allows clicky buttons */}); err != nil {
			log.Println(err)
			return
		}
		if err := curr.Operative.conn.WriteMessage(websocket.TextMessage, clickyBoard.Bytes()); err != nil {
			log.Println(err)
			return
		}

		// operative can make clue.Number + 1 guesses or less, if he chooses to end guessing
		// if he guesses incorrect color, his turn ends too
		for range clue.Number + 1 {
			// reading the sent guess
			_, move, err := curr.Operative.conn.ReadMessage()
			if err != nil {
				log.Println(err)
				continue
			}

			var guess *Guess
			if err := json.Unmarshal(move, &guess); err != nil {
				log.Println(err)
				return
			}
			// for debugging purposes
			log.Println(guess)

			// validating the move
			pLock.RLock()
			if player, ok := players[guess.PlayerID]; !ok || player.ID != curr.Operative.ID || player != curr.Operative {
				log.Println("Invalid player")
				pLock.RUnlock()
				continue
			}
			pLock.RUnlock()

			if guess.EndGuessing == true {
				if err := curr.Operative.conn.WriteMessage(websocket.TextMessage,
					[]byte(`<span id="end-guessing"></span>`)); err != nil {
					log.Println(err)
					return
				}
				break
			}

			if guess.Col > Size || guess.Row > Size || guess.Col < 0 || guess.Row < 0 {
				log.Println("Invalid cell")
				continue
			}
			// maybe I should add validation of the word itself? e.g. is the guessed word in this cell

			// sending the open cell to everyone after validating the move
			cell := &game.Board[guess.Row][guess.Col]

			if cell.IsOpen {
				log.Println("Cell is already open")
				continue
			}
			cell.IsOpen = true

			// open cell template usage
			openCellTmpl := template.Must(template.New("open-cell").Parse(OpenCell))
			var openCell bytes.Buffer
			if err := openCellTmpl.Execute(&openCell, struct {
				Col   int
				Row   int
				Color string
				Word  string
			}{guess.Col, guess.Row, cell.Color, cell.Word}); err != nil {
				log.Println(err)
				return
			}
			playersHere := []*Player{
				game.Blue.Operative,
				game.Blue.Spymaster,
				game.Red.Operative,
				game.Red.Spymaster,
			}
			for _, player := range playersHere {
				if player == nil {
					log.Println("somehow one of the players isn't here")
					return
				}
				if err := player.conn.WriteMessage(websocket.TextMessage, openCell.Bytes()); err != nil {
					log.Println(err)
					return
				}
			}

			for connection := range game.lobby {
				if connection == nil {
					continue
				}
				if err := connection.WriteMessage(websocket.TextMessage, openCell.Bytes()); err != nil {
					log.Println(err)
					delete(game.lobby, connection)
					return
				}
			}

			// evaluating the move
			var wrong bool
			if cell.Color == curr.Operative.Team {
				curr.WordsLeft--
				if curr.WordsLeft == 0 {
					game.Winner = curr
				}
			} else {
				wrong = true
				if cell.Color == next.Operative.Team {
					next.WordsLeft--
					if next.WordsLeft == 0 {
						game.Winner = next
					}
				} else if cell.Color == Black {
					game.Winner = next
				}
			}

			// if someone has won
			if game.Winner != nil {
				// send everyone the spymaster board
				boardTmpl := template.Must(template.New("board").
					Funcs(template.FuncMap{
						"map": MapTempl,
						"safe": func(s string) template.CSS {
							return template.CSS(s)
						},
					}).
					ParseFiles("board.html"))

				// after game ends, everyone should see the remaining words to have a chat about it
				var spymasterBoard bytes.Buffer
				if err := boardTmpl.Execute(&spymasterBoard, struct {
					Role  string
					Board *Board
					Turn  bool
				}{Spymaster, game.Board, false}); err != nil {
					log.Println(err)
					return
				}
				playersHere := []*Player{
					game.Blue.Operative,
					game.Blue.Spymaster,
					game.Red.Operative,
					game.Red.Spymaster,
				}
				for _, player := range playersHere {
					if player == nil {
						log.Println("somehow one of the players isn't here")
						return
					}
					if err := player.conn.WriteMessage(websocket.TextMessage, spymasterBoard.Bytes()); err != nil {
						log.Println(err)
						return
					}
				}

				for connection := range game.lobby {
					if connection == nil {
						continue
					}
					if err := connection.WriteMessage(websocket.TextMessage, spymasterBoard.Bytes()); err != nil {
						log.Println(err)
						delete(game.lobby, connection)
						return
					}
				}

				// send the info about who won
				winnerTmpl := template.Must(template.New("winner").Parse(Winner))
				var winner bytes.Buffer
				if err := winnerTmpl.Execute(&winner, struct {
					Color string
				}{game.Winner.Operative.Team}); err != nil {
					log.Println(err)
					return
				}

				for _, player := range playersHere {
					if player == nil {
						log.Println("somehow one of the players isn't here")
						return
					}
					if err := player.conn.WriteMessage(websocket.TextMessage, winner.Bytes()); err != nil {
						log.Println(err)
						return
					}
				}

				for connection := range game.lobby {
					if connection == nil {
						continue
					}
					if err := connection.WriteMessage(websocket.TextMessage, winner.Bytes()); err != nil {
						log.Println(err)
						delete(game.lobby, connection)
					}
				}

				// finally! end of the game
				game.ended = true
			}

			if wrong {
				break
			}
		}

		// remove the endguessing button
		if err := curr.Operative.conn.WriteMessage(websocket.TextMessage,
			[]byte(`<span id="end-guessing"></span>`)); err != nil {
			log.Println(err)
			return
		}

		// resetting everything
		// sending the empty clue to everyone
		next = curr
		game.changeTurn()
		game.Clue = nil
		game.giveClue()
	}
}

const Winner = `
<div id="winner">
    <br>
    <span style="color:{{.Color}};">{{.Color}}</span> team won!
</div>
`

func (game *Game) changeTurn() {
	if game.Turn == &game.Blue {
		game.Turn = &game.Red
	} else {
		game.Turn = &game.Blue
	}
}

func (game *Game) sendBoardToEveryone() {
	// render two separate boards for two kinds of players
	boardTmpl := template.Must(template.New("board").
		Funcs(template.FuncMap{
			"map": MapTempl,
			"safe": func(s string) template.CSS {
				return template.CSS(s)
			},
		}).
		ParseFiles("board.html"))

	var spymasterBoard, operativeBoard bytes.Buffer
	if err := boardTmpl.Execute(&spymasterBoard, struct {
		Role  string
		Board *Board
		Turn  bool
	}{Spymaster, game.Board, false}); err != nil {
		log.Println(err)
		return
	}
	if err := boardTmpl.Execute(&operativeBoard, struct {
		Role  string
		Board *Board
		Turn  bool
	}{Operative, game.Board, false}); err != nil {
		log.Println(err)
		return
	}

	// send those boards depending on the player role (observers in the lobby get the operative's view)
	playersHere := []*Player{
		game.Blue.Operative,
		game.Blue.Spymaster,
		game.Red.Operative,
		game.Red.Spymaster,
	}
	for _, player := range playersHere {
		if player == nil {
			log.Println("somehow one of the players isn't here")
			return
		}
		var resp bytes.Buffer
		if player.Role == Operative {
			resp = operativeBoard
		} else {
			resp = spymasterBoard
		}
		if err := player.conn.WriteMessage(websocket.TextMessage, resp.Bytes()); err != nil {
			log.Println(err)
			return
		}
	}

	for connection := range game.lobby {
		if connection == nil {
			continue
		}
		if err := connection.WriteMessage(websocket.TextMessage, operativeBoard.Bytes()); err != nil {
			log.Println(err)
			delete(game.lobby, connection)
			return
		}
	}
}

func (game *Game) giveClue() {
	clueTmpl := template.Must(template.New("clue").ParseFiles("clue.html"))
	var clue bytes.Buffer
	if err := clueTmpl.Execute(&clue, game.Clue); err != nil {
		log.Println(err)
		return
	}

	playersHere := []*Player{
		game.Blue.Operative,
		game.Blue.Spymaster,
		game.Red.Operative,
		game.Red.Spymaster,
	}
	for _, player := range playersHere {
		if player == nil {
			log.Println("somehow one of the players isn't here")
			return
		}
		if err := player.conn.WriteMessage(websocket.TextMessage, clue.Bytes()); err != nil {
			log.Println(err)
			return
		}
	}

	for connection := range game.lobby {
		if connection == nil {
			continue
		}
		if err := connection.WriteMessage(websocket.TextMessage, clue.Bytes()); err != nil {
			log.Println(err)
			delete(game.lobby, connection)
			return
		}
	}
}
