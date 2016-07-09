package main

import (
	"errors"
	"fmt"
	"github.com/bwmarrin/discordgo"
	"github.com/loganjspears/joker/hand"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/loganjspears/joker/table"
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

	Channel    string
	MessageEvt chan *PlayerMessage
	Running    bool

	hasSentCards      bool
	printedBoardState int
	stopAfterDone     bool
}

func (t *Table) HandleMessage(m *discordgo.MessageCreate) {

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

// Incase there any messages pending on the channel, empty it when we stop running
func (t *Table) emptyChannel() {
	for {
		select {
		case <-t.MessageEvt:
			continue
		default:
			return
		}
	}
}

func (t *Table) run() {
	for {
		results, done, err := t.Table.Next()
		if done || (results != nil && t.stopAfterDone) {
			if t.stopAfterDone {
				for _, v := range t.Table.Players() {
					cast := v.Player().(*TablePlayer)
					GiveMoney(cast.Id, cast.Name, v.Chips())
				}
				// Mark ready
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
			go SurelySend(t.Channel, "Results:\n"+printResults(t.Table, results)+"\nStarting next hand in 5 seconds")

		}
		if results == nil {
			t.MaybeSendTable()
		}
		if err != nil {
			log.Println("Error", err)
			go SurelySend(t.Channel, "Error "+err.Error())
		}

		for _, v := range t.Table.Players() {
			player := v.Player().(*TablePlayer)
			if player.LeaveAfterFold && (player.foldedAndReadyToLeave || results != nil) {
				money := v.Chips()
				t.Table.Stand(v.Player())

				go GiveMoney(player.Id, player.Name, money)
				go SurelySend(t.Channel, fmt.Sprintf("%s stood up", player.Name))
			}
		}

		// Sleep at the end and maybe send cards
		if results != nil {
			t.Unlock()
			time.Sleep(time.Second * 5) // take a nap zzzzz
			t.Lock()
		} else if !t.hasSentCards {
			t.SendPlayerCards()
			t.hasSentCards = true
		}
	}
}

func GiveMoney(id, name string, money int) {
	player := playerManager.GetCreatePlayer(id, name)
	player.Lock()
	player.Money += money
	player.Unlock()
}

func (t *Table) SendPlayerCards() {
	for _, player := range t.Table.Players() {

		tablePlayer, ok := player.Player().(*TablePlayer)
		if !ok {
			panic("Failed casting to tableplayer??")
		}
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

		go SurelySend(tablePlayer.PrivateChannel, fmt.Sprintf("Your hand\n```\n%s\n```\n%s", createAsciiCards(cards, " "), cardsStr))
	}
}

type TablePlayer struct {
	Table          *Table
	Id             string
	Name           string
	PrivateChannel string

	LeaveAfterFold        bool
	foldedAndReadyToLeave bool
}

type PlayerMessage struct {
	From    string
	Message string
}

func (p *TablePlayer) ID() string {
	return p.Id
}

func (p *TablePlayer) FromID(id string) (table.Player, error) {
	return &TablePlayer{Id: p.Id}, nil
}

func (p *TablePlayer) Action() (table.Action, int) {

	// get action from input
	// actions := []string{}
	// for _, a := range tbl.ValidActions() {
	// 	actions = append(actions, strings.ToLower(string(a)))
	// }

	current := p.Table.Table.CurrentPlayer()

	validActions := p.Table.Table.ValidActions()
	actions := ""
	for k, v := range validActions {
		if k != 0 {
			actions += ", "
		}

		actions += string(v)
		if v == table.Call {
			actions += fmt.Sprintf(" (%d)", p.Table.Table.Outstanding())
		}
	}

	go SurelySend(p.Table.Channel, fmt.Sprintf("<@%s>'s Turn, Chips: %d, MinRaise: %d, MaxRaise: %d, Actions: **%s**, Pot: **%d**",
		p.Id, current.Chips(), p.Table.Table.MinRaise()-p.Table.Table.Outstanding(), p.Table.Table.MaxRaise()-p.Table.Table.Outstanding(), actions, p.Table.Table.Pot().Chips()))

	// Fold automatically after 30 seconds
	after := time.After(time.Minute * 3)
	for {
		cmd := ""

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
				cmd = "fold"
			} else {
				cmd = "check"
			}
		case pm := <-p.Table.MessageEvt:
			if pm.From != p.Id {
				continue
			}
			cmd = pm.Message
		}
		p.Table.Lock()

		split := strings.Fields(cmd)
		if len(split) < 1 {
			continue
		}

		// parse action
		action, err := actionFromInput(strings.ToLower(split[0]))
		if err != nil {
			continue
		}

		found := false
		for _, v := range validActions {
			if v == action {
				found = true
			}
		}

		// Not a valid action
		if !found {
			go SurelySend(p.Table.Channel, "You can't do that")
			continue
		}

		if !(action == table.Bet || action == table.Raise) {
			if action == table.Fold && p.LeaveAfterFold {
				p.foldedAndReadyToLeave = true
			}
			return action, 0
		}

		if len(split) < 2 {
			go SurelySend(p.Table.Channel, "Try again by also specifying amount")
			continue
		}

		chips, err := strconv.ParseInt(split[1], 10, 64)
		if err != nil {
			go SurelySend(p.Table.Channel, "Failed parsing number >:O")
			continue
		}

		if action == table.Raise {
			chips += int64(p.Table.Table.Outstanding())
		}

		return action, int(chips)
	}

	return table.Fold, 0
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

func actionFromInput(input string) (table.Action, error) {
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