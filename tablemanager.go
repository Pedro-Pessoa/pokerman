package main

import (
	"errors"
	"fmt"
	"github.com/bwmarrin/discordgo"
	"github.com/jonas747/joker/hand"
	"github.com/jonas747/joker/table"
	"log"
	"sync"
)

type StartEvt struct {
	PlayerId string
	Channel  string
}

type CreateTableEvt struct {
	PlayerID       string
	Name           string
	PrivateChannel string
	Channel        string
	BuyIn          int
	Small          int
	Big            int
}
type AddPlayerEvt struct {
	PlayerID       string
	Name           string
	BuyIn          int
	Channel        string
	PrivateChannel string
}

type RemovePlayerEvt struct {
	PlayerID string
	Channel  string
}

type DestroyTableEvt struct {
	Channel string
}

type PrintInfoEvt struct {
	Channel string
}

type ChangeSettingsEvt struct {
	PlayerId string
	settings map[string]interface{}
}

type StopEvt struct {
	wg *sync.WaitGroup
}

type TableManager struct {
	tables []*Table

	EvtChan chan interface{}

	stopWg   *sync.WaitGroup
	stopping bool
}

var ErrStop = errors.New("Stopping")

func (t *TableManager) Run() {
	for {
		evt := <-t.EvtChan
		stopEvt, ok := evt.(*StopEvt)
		if ok {
			t.GracefullShutdown()
			t.stopWg = stopEvt.wg
			t.stopping = true

			if len(t.tables) < 1 {
				t.stopWg.Done()
				return
			}

		} else {
			err := t.HandleEvent(evt)
			if err != nil {
				if err == ErrStop {
					log.Println("Tablemanager is done waiting")
					t.stopWg.Done()
					return
				} else {
					log.Println("Error hadnling TableManager event:", err)
				}
			}
		}
	}
}

func (t *TableManager) GracefullShutdown() {
	// Set tables to last round mode and wait till rounds are over
	for _, v := range t.tables {
		v.Lock()

		if !v.Running {
			for _, p := range v.Table.Players() {
				cast := p.Player().(*TablePlayer)
				v.Unlock()
				GiveMoney(cast.Id, cast.Name, p.Chips())
				v.Lock()
			}
			t.RemoveTable(v.Channel)
		} else {
			v.stopAfterDone = true
		}

		v.Unlock()
	}
}

func (t *TableManager) HandleEvent(e interface{}) error {
	switch evt := e.(type) {
	case *discordgo.MessageCreate:
		authorId := evt.Author.ID
		for _, tbl := range t.tables {
			tbl.Lock()
			if tbl.Running {
				if tbl.Table.CurrentPlayer().Player().ID() == authorId {
					go func() {
						tbl.MessageEvt <- &PlayerMessage{From: authorId, Message: evt.Content}
					}()
					tbl.Unlock()
					break
				}
			}
			tbl.Unlock()
		}
	case *CreateTableEvt:

		// Check if there is already a table in this channel
		if t.GetTable(evt.Channel) != nil {
			go SurelySend(evt.Channel, "There's already a game running in this channel")
			return nil
		}

		if evt.Small < 1 {
			evt.Small = 1
		}
		if evt.Big < 1 {
			evt.Big = 2
		}

		opts := table.Config{
			Game:       table.Holdem,
			Limit:      table.NoLimit,
			Stakes:     table.Stakes{SmallBet: evt.Small, BigBet: evt.Big, Ante: 0},
			NumOfSeats: 10,
		}
		coreTable := table.New(opts, hand.NewDealer())

		tbl := &Table{
			Table:      coreTable,
			Channel:    evt.Channel,
			Owner:      evt.PlayerID,
			OwnerName:  evt.Name,
			MessageEvt: make(chan *PlayerMessage),
		}

		player := playerManager.GetCreatePlayer(evt.PlayerID, evt.Name)
		player.Lock()
		if evt.BuyIn > player.Money {
			go SurelySend(evt.Channel, "You don't have enough money")
			player.Unlock()
			return nil
		}
		player.Money -= evt.BuyIn
		player.Unlock()

		tp := &TablePlayer{
			Id:             evt.PlayerID,
			PrivateChannel: evt.PrivateChannel,
			Table:          tbl,
			Name:           evt.Name,
		}

		err := tbl.Table.Sit(tp, 0, evt.BuyIn)
		if err != nil {
			log.Println("Failed to sit at own table?!?!?", err)
			go SurelySend(evt.Channel, "Failed to sit at own table.. "+err.Error())
			player.Lock()
			player.Money += evt.BuyIn
			player.Unlock()
			return nil
		}
		t.tables = append(t.tables, tbl)
		go SurelySend(evt.Channel, "Created table, get atleast 2 people to join before you can start")
	case *AddPlayerEvt:
		tbl := t.GetTable(evt.Channel)
		if tbl == nil {
			go SurelySend(evt.Channel, "No table here")
			return nil
		}

		tp := &TablePlayer{
			Id:             evt.PlayerID,
			PrivateChannel: evt.PrivateChannel,
			Name:           evt.Name,
		}

		player := playerManager.GetCreatePlayer(evt.PlayerID, evt.Name)
		player.Lock()
		if player.Money < evt.BuyIn {
			go SurelySend(evt.Channel, "Not enough money to join")
			player.Unlock()
			return nil
		}

		// Subtract buyin money
		player.Money -= evt.BuyIn
		player.Unlock()

		foundSeat := false
		tbl.Lock()
		for i := 0; i < tbl.Table.NumOfSeats(); i++ {
			err := tbl.Table.Sit(tp, i, evt.BuyIn)
			if err == nil {
				foundSeat = true
				go SurelySend(evt.Channel, evt.Name+" Joined the table")
				break
			} else if err != table.ErrSeatOccupied {
				go SurelySend(evt.Channel, "Error joining table: "+err.Error())
				break
			}
		}
		if !foundSeat {
			go SurelySend(evt.Channel, "No available seats :(")
			player.Lock()
			player.Money += evt.BuyIn
			player.Unlock()
		} else {
			tp.Table = tbl
		}
		tbl.Unlock()

	case *RemovePlayerEvt:
		tbl := t.GetTable(evt.Channel)
		if tbl == nil {
			return nil
		}

		tbl.Lock()
		var p *table.PlayerState
		for _, player := range tbl.Table.Players() {
			if player.Player().ID() == evt.PlayerID {
				p = player
				break
			}
		}
		if p == nil {
			go SurelySend(evt.Channel, "You're not in the game")
			return nil
		}

		playerCast := p.Player().(*TablePlayer)

		if tbl.Running && !p.Out() {
			playerCast.LeaveAfterFold = true
			go SurelySend(evt.Channel, "Leaving after round (fold if you just want to begone)")
			tbl.Unlock()
		} else {

			tbl.Table.Stand(p.Player())
			go SurelySend(evt.Channel, "You stoop up")
			tbl.CheckReplaceOwner()
			// Destroy it
			if len(tbl.Table.Players()) < 1 {
				t.RemoveTable(tbl.Channel)
			}
			tbl.Unlock()

			player := playerManager.GetCreatePlayer(evt.PlayerID, playerCast.Name)
			player.Lock()
			player.Money += p.Chips()
			player.Unlock()
		}
	case *DestroyTableEvt:
		t.RemoveTable(evt.Channel)
		if t.stopping {
			if len(t.tables) == 0 {
				return ErrStop
			} else {
				log.Printf("%d tables left before stop\n", len(t.tables))
			}

		}
	case *StartEvt:
		for _, tbl := range t.tables {
			if tbl.Channel == evt.Channel {
				tbl.Lock()
				if !tbl.Running && len(tbl.Table.Players()) >= 2 {
					go SurelySend(evt.Channel, "Starting")
					go tbl.Run()
				}
				tbl.Unlock()
			}
		}
	case *PrintInfoEvt:
		tbl := t.GetTable(evt.Channel)

		if tbl != nil {
			tbl.Lock()
			t.SendTableInfo(evt.Channel, tbl)
			tbl.Unlock()
		}

	case *ChangeSettingsEvt:
	}

	return nil
}

func (t *TableManager) RemoveTable(channel string) {
	for k, tbl := range t.tables {
		if tbl.Channel == channel {
			t.tables = append(t.tables[:k], t.tables[k+1:]...)
			go SurelySend(channel, "Destroyed table baibai")
			break
		}
	}
}

func (t *TableManager) SendTableInfo(channel string, tbl *Table) {
	stakes := tbl.Table.Stakes()

	tableConfigStr := fmt.Sprintf("Table Config:\n - Owner: %s\n - Game: **%s**\n -  Seats: **%d**\n - Limit: **%s**\n - Stakes (small, big, ante): **%d**, **%d**, **%d**\n",
		tbl.OwnerName, tbl.Table.Game().String(), tbl.Table.NumOfSeats(), tbl.Table.Limit(), stakes.SmallBet, stakes.BigBet, stakes.Ante)

	playersStr := ""

	for k, v := range tbl.Table.Players() {
		tablePlayer := v.Player().(*TablePlayer)
		playersStr += fmt.Sprintf("Seat [%d] %s: $%d\n", k, tablePlayer.Name, v.Chips())
	}

	go SurelySend(channel, tableConfigStr+"\n"+playersStr)
}

func (t *TableManager) GetTable(channel string) *Table {
	for _, tbl := range t.tables {
		if tbl.Channel == channel {
			return tbl
		}
	}
	return nil
}
