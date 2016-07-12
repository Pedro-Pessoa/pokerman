package main

import (
	"errors"
	"fmt"
	"github.com/jonas747/joker/hand"
	"github.com/jonas747/joker/table"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	fold  = "fold"
	check = "check"
	call  = "call"
	bet   = "bet"
	raise = "raise"
)

type Table struct {
	sync.Mutex
	Table   *table.Table
	Manager *TableManager

	Owner     string
	OwnerName string

	Channel       string
	ActionEvt     chan *ActionEvt
	Running       bool
	WaitingToJoin []*TablePlayer

	hasSentCards      bool
	printedBoardState int
	stopAfterDone     bool
}

func (t *Table) Run() {
	t.Lock()

	t.Running = true
	go SurelySend(t.Channel, "Started table")

	t.run()

	t.Running = false

	t.Unlock()
	go SurelySend(t.Channel, "Stopped table")
	t.emptyChannel()
}

// Incase there any messages pending on the channel, empty it when we stop running to avoid deadlocks
func (t *Table) emptyChannel() {
	for {
		select {
		case <-t.ActionEvt:
			continue
		default:
			return
		}
	}
}

// Run the tableee
func (t *Table) run() {
	for {
		results, done, err := t.Table.Next()
		if done || (results != nil && t.stopAfterDone) {
			if t.stopAfterDone {
				for _, v := range t.Table.Players() {
					cast := v.Player().(*TablePlayer)
					GiveMoney(cast.Id, cast.Name, v.Chips())
				}
				// Remove it from tablemanager
				tableManager.EvtChan <- &DestroyTableEvt{Channel: t.Channel}
			}

			if results != nil {
				msgText := "Not enough players for another hand, stopping.."
				if t.stopAfterDone {
					msgText = "Bot is shutting down"
				}
				go SurelySend(t.Channel, "Results:\n"+printResults(t.Table, results)+"\n"+msgText)
			}

			return
		}

		if results != nil {
			t.hasSentCards = false
			t.printedBoardState = 0
			go SurelySend(t.Channel, "Results:\n"+printResults(t.Table, results)+"\nStarting next hand in 10 seconds")
		}

		if err != nil {
			log.Println("Error", err)
			go SurelySend(t.Channel, "Error "+err.Error())
		}

		if results == nil {
			t.MaybeSendTable()
		}

		for _, v := range t.Table.Players() {
			player := v.Player().(*TablePlayer)
			if player.LeaveAfterFold && (player.foldedAndReadyToLeave || results != nil) {
				money := v.Chips()
				t.Table.Stand(v.Player())
				t.CheckReplaceOwner()

				go GiveMoney(player.Id, player.Name, money)
				go SurelySend(t.Channel, fmt.Sprintf("%s stood up", player.Name))
			}
		}

		// Sleep at the end and maybe send cards
		if results != nil {
			t.Unlock()
			time.Sleep(time.Second * 10) // take a nap zzzzz
			t.Lock()
		} else if !t.hasSentCards {
			t.SendPlayerCards()
			t.hasSentCards = true
		}
	}
}

func (t *Table) SendPlayerCards() {
	for _, player := range t.Table.Players() {
		if player.Out() || player.Chips() < 1 {
			continue
		}

		tablePlayer, ok := player.Player().(*TablePlayer)
		if !ok {
			panic("Failed casting to tableplayer??")
		}
		tablePlayer.SendCards(player)
	}
}

// Checks if the owner of the table is at the table, if not replace him
func (t *Table) CheckReplaceOwner() {
	for _, p := range t.Table.Players() {
		id := p.Player().ID()
		if id == t.Owner {
			return // Owner is at the table
		}
	}

	// Owner not at the table, assign a new one
	for _, p := range t.Table.Players() {
		t.Owner = p.Player().ID()
		cast := p.Player().(*TablePlayer)
		t.OwnerName = cast.Name
		go SurelySend(t.Channel, "New owner for table: "+cast.Name)
		return
	}
}

// Sends the table if it has changed
func (t *Table) MaybeSendTable() {

	board := t.Table.Board()
	if len(board) > 0 && t.printedBoardState < len(board) {

		cardsStr := "["
		for k, c := range board {
			if k != 0 {
				cardsStr += ", "
			}
			cardsStr += string(c.Rank()) + " " + string(c.Suit())
		}
		cardsStr += "]"

		go SurelySend(t.Channel, fmt.Sprintf("Board\n```\n%s\n```\n%s", createAsciiCards(board, " "), cardsStr))
		t.printedBoardState = len(board)
	}
}

func (t *Table) ChangeSetting(key string, strVal string) {

	trimmed := strings.TrimSpace(strVal)

	floatVal, _ := strconv.ParseFloat(trimmed, 64)
	intVal := int(floatVal)

	currentConfig := t.Table.Config()

	switch strings.ToLower(key) {
	case "smallbet", "small":
		currentConfig.Stakes.SmallBet = intVal
	case "bigbet", "big":
		currentConfig.Stakes.BigBet = intVal
	case "ante":
		currentConfig.Stakes.Ante = intVal
	case "limit":
		switch strings.ToLower(strVal) {
		case "nl", "no", "nolimit":
			currentConfig.Limit = table.NoLimit
		case "fl", "fixed", "fixedlimit":
			currentConfig.Limit = table.FixedLimit
		case "pl", "pot", "potlimit":
			currentConfig.Limit = table.PotLimit
		}
	case "seats":
		currentConfig.NumOfSeats = intVal
	case "game":
		go SurelySend(t.Channel, "TODO")
	}

	t.Table.SetConfig(currentConfig)
}

type TablePlayer struct {
	Table          *Table
	Id             string
	Name           string
	PrivateChannel string
	LeaveAfterFold bool

	foldedAndReadyToLeave bool
}

func (p *TablePlayer) ID() string {
	return p.Id
}

func (p *TablePlayer) FromID(id string) (table.Player, error) {
	return &TablePlayer{Id: p.Id}, nil
}

func (p *TablePlayer) Action() (table.Action, int) {

	current := p.Table.Table.CurrentPlayer()

	outstanding := p.Table.Table.Outstanding()

	validActions := p.Table.Table.ValidActions()
	actions := ""
	for k, v := range validActions {
		if k != 0 {
			actions += ", "
		}

		actions += string(v)
		if v == table.Call {
			actions += fmt.Sprintf(" (%d)", outstanding)
		}
	}

	min := p.Table.Table.MinRaise() // - outstanding
	max := p.Table.Table.MaxRaise() // - outstanding

	go SurelySend(p.Table.Channel, fmt.Sprintf("<@%s>'s Turn, Chips: %d, MinRaise: %d, MaxRaise: %d, Actions: **%s**, Pot: **%d**",
		p.Id, current.Chips(), min, max, actions, p.Table.Table.Pot().Chips()))

	// Fold automatically after 30 seconds
	after := time.After(time.Minute * 3)
	for {

		var action *Action
		p.Table.Unlock()
		select {
		case <-after:
			canFold := false
			for _, v := range validActions {
				if v == table.Fold {
					canFold = true
					break
				}
			}

			if canFold {
				action = &Action{IsTableAction: true, TableAction: table.Fold}
			} else {
				action = &Action{IsTableAction: true, TableAction: table.Check}
			}
		case actionEvt := <-p.Table.ActionEvt:
			if actionEvt.PlayerID != p.Id {
				p.Table.Lock()
				continue
			}

			action = actionEvt.Action
		}
		p.Table.Lock()

		// parse action
		found := false
		if action.IsTableAction {
			for _, v := range validActions {
				if v == action.TableAction {
					found = true
				}
			}
		}

		chipAmountSet := false
		chipAmount := 0

		if !found {
			if action.AllIn {
				action = &Action{IsTableAction: true}
				if outstanding >= current.Chips() {
					action.TableAction = table.Call
				} else {
					if outstanding > 0 { // Raise
						action.TableAction = table.Raise
					} else {
						action.TableAction = table.Bet
					}
					chipAmount = max
				}
				found = true
			}
		}

		// Not a valid action
		if !found {
			go SurelySend(p.Table.Channel, "You can't do that")
			continue
		}

		if !(action.TableAction == table.Bet || action.TableAction == table.Raise) {
			if action.TableAction == table.Fold && p.LeaveAfterFold {
				p.foldedAndReadyToLeave = true
			}
			return action.TableAction, 0
		}

		if !chipAmountSet {

			if action.RestMessage == "" {
				go SurelySend(p.Table.Channel, "Try again by also specifying amount")
				continue
			}

			chips, err := strconv.ParseInt(action.RestMessage, 10, 64)
			if err != nil {
				go SurelySend(p.Table.Channel, "Failed parsing number >:O")
				continue
			}
			if chips <= 0 {
				go SurelySend(p.Table.Channel, "Can't raise/bet anythign less then 1 >:(")
				continue
			}

			chipAmount = int(chips)

		}

		return action.TableAction, chipAmount
	}

	return table.Fold, 0
}

func (p *TablePlayer) SendCards(player *table.PlayerState) {
	holeCards := player.HoleCards()
	cards := make([]*hand.Card, len(holeCards))
	cardsStr := "["
	for k, hc := range holeCards {
		if k != 0 {
			cardsStr += ", "
		}
		cardsStr += string(hc.Card.Rank()) + " " + string(hc.Card.Suit())
		cards[k] = hc.Card
	}
	cardsStr += "]"
	go SurelySend(p.PrivateChannel, fmt.Sprintf("Your hand\n```\n%s\n```\n%s", createAsciiCards(cards, " "), cardsStr))
}

func createAsciiCards(cards []*hand.Card, spacing string) string {
	lines := make([][]string, 5)
	for _, card := range cards {
		rendered := createAsciiCard(card)
		split := strings.Split(rendered, "\n")
		for k, s := range split {
			lines[k] = append(lines[k], s)
		}
	}
	out := ""
	for _, line := range lines {
		for k, c := range line {
			if k != 0 {
				out += spacing
			}
			out += c
		}
		out += "\n"
	}
	return out
}

func createAsciiCard(card *hand.Card) string {
	format := `┌───┐
│%s  │
│ %s │
│  %s│
└───┘`
	return fmt.Sprintf(format, string(card.Suit()), string(card.Rank()), string(card.Suit()))
}

func printResults(tbl *table.Table, results map[int][]*table.Result) string {
	out := ""
	players := tbl.Players()
	for seat, resultList := range results {
		for _, result := range resultList {
			tablePlayer := players[seat].Player().(*TablePlayer)
			out += fmt.Sprint(tablePlayer.Name+":", result) + "\n"
		}
	}
	return out
}

var AllInRegex = regexp.MustCompile("^a+ll+ i+n")

type Action struct {
	IsTableAction bool
	TableAction   table.Action

	AllIn bool

	RestMessage string
}

func GetAction(input string) *Action {
	lower := strings.ToLower(input)

	split := strings.SplitN(lower, " ", 2)
	if len(split) < 1 {
		return nil
	}

	ta, err := TableAction(lower[0])
	if err == nil {
		rest := ""
		if len(split) > 1 {
			rest = split[1]
		}

		return &Action{IsTableAction: true, TableAction: ta, RestMessage: rest}
	}

	if AllInRegex.MatchString(lower) {
		return &Action{AllIn: true}
	}

	return nil
}

func TableAction(input string) (table.Action, error) {
	switch input {
	case fold:
		return table.Fold, nil
	case check:
		return table.Check, nil
	case call:
		return table.Call, nil
	case bet:
		return table.Bet, nil
	case raise:
		return table.Raise, nil
	}
	return table.Fold, errors.New(input + " is not an action.")
}
