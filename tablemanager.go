package main

import (
	"errors"
	"fmt"
	"github.com/jonas747/joker/hand"
	"github.com/jonas747/joker/table"
	"log"
	"sync"
)

type ActionEvt struct {
	PlayerID string
	Channel  string
	Action   *Action
}

type StartEvt struct {
	PlayerID string
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
	PlayerID string
	Channel  string
	Settings map[string]string
}

type StopTableEvt struct {
	PlayerID string
	Channel  string
}

type KickPlayerEvt struct {
	PlayerID     string // Sender
	KickPlayerID string // Kicked player
	Channel      string
}

type BanPlayerEvt struct {
	PlayerID    string
	BanPlayerID string
	Channel     string
}

type StopEvt struct {
	wg *sync.WaitGroup
}

// Table manager runs in it's own goroutine and manages all tables
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
			v.serverShuttingDown = true
			go SurelySend(v.Channel, "Bot is shutting down after all tables has completed...")
		}

		v.Unlock()
	}
}

func (t *TableManager) HandleAction(tbl *Table, evt *ActionEvt) {
	tbl.Lock()
	defer tbl.Unlock()

	if !tbl.Running {
		return
	}

	if tbl.Table.CurrentPlayer() == nil {
		return
	}

	if tbl.Table.CurrentPlayer().Player().ID() == evt.PlayerID {
		go func() {
			tbl.ActionEvt <- evt
		}()
	}
}

func (t *TableManager) HandleEvent(e interface{}) error {
	switch evt := e.(type) {
	case *ActionEvt:
		tbl := t.GetTable(evt.Channel)
		if tbl == nil {
			return nil
		}

		t.HandleAction(tbl, evt)
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
			Manager:   t,
			Table:     coreTable,
			Channel:   evt.Channel,
			Owner:     evt.PlayerID,
			OwnerName: evt.Name,
			ActionEvt: make(chan *ActionEvt),
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
		tbl := t.requireTable(evt.Channel)
		if tbl == nil {
			return nil
		}

		tp := &TablePlayer{
			Id:             evt.PlayerID,
			PrivateChannel: evt.PrivateChannel,
			Name:           evt.Name,
		}

		if tbl.IsPlayerBanned(evt.PlayerID) {
			go SurelySend(evt.Channel, "You're banned from this table")
			return nil
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
			tbl.Unlock()
			go SurelySend(evt.Channel, "No available seats :(")
			player.Lock()
			player.Money += evt.BuyIn
			player.Unlock()
		} else {
			tp.Table = tbl
			tbl.Unlock()
		}

	case *RemovePlayerEvt:
		tbl := t.requireTable(evt.Channel)
		if tbl == nil {
			return nil
		}

		tbl.Lock()
		tbl.RemovePlayer(evt.PlayerID, false)
		tbl.Unlock()
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
		tbl := t.requireTable(evt.Channel)
		if tbl != nil {
			tbl.Lock()
			if !tbl.Running && len(tbl.Table.Players()) >= 2 {
				go SurelySend(evt.Channel, "Starting")
				go tbl.Run()
			}
			tbl.Unlock()
		}
	case *PrintInfoEvt:
		tbl := t.requireTable(evt.Channel)

		if tbl != nil {
			tbl.Lock()
			t.SendTableInfo(evt.Channel, tbl)
			tbl.Unlock()
		}

	case *ChangeSettingsEvt:
		tbl := t.requireTable(evt.Channel)
		if tbl == nil {
			return nil
		}

		tbl.Lock()
		if tbl.Running {
			go SurelySend(evt.Channel, "Can't change setting while table is running")
			tbl.Unlock()
			return nil
		}

		if !t.requireOwner(tbl, evt.PlayerID) {
			tbl.Unlock()
			return nil
		}

		for key, val := range evt.Settings {
			tbl.ChangeSetting(key, val)
		}
		t.SendTableInfo(evt.Channel, tbl)
		tbl.Unlock()
	case *StopTableEvt:
		tbl := t.requireTable(evt.Channel)
		if tbl == nil {
			return nil
		}

		tbl.Lock()
		if t.requireOwner(tbl, evt.PlayerID) {
			tbl.stopAfterDone = true
		}
		tbl.Unlock()
	case *KickPlayerEvt:
		tbl := t.requireTable(evt.Channel)
		if tbl == nil {
			return nil
		}
		tbl.Lock()
		if t.requireOwner(tbl, evt.PlayerID) {
			tbl.RemovePlayer(evt.KickPlayerID, true)
		}
		tbl.Unlock()
	case *BanPlayerEvt:
		tbl := t.requireTable(evt.Channel)
		if tbl == nil {
			return nil
		}

		tbl.Lock()
		if t.requireOwner(tbl, evt.PlayerID) {
			tbl.RemovePlayer(evt.BanPlayerID, true)
			if !tbl.IsPlayerBanned(evt.PlayerID) {
				tbl.BannedPlayers = append(tbl.BannedPlayers, evt.PlayerID)
			}
		}
		tbl.Unlock()
	}

	return nil
}

func (t *TableManager) requireOwner(tbl *Table, id string) bool {
	if tbl.Owner != id {
		go SurelySend(tbl.Channel, "Only owner of table can do this")
		return false
	}

	return true
}

// If there is no table there will return nil and send a message in the channel stating no table was found
func (t *TableManager) requireTable(channel string) *Table {
	tbl := t.GetTable(channel)
	if tbl == nil {
		go SurelySend(channel, "No table in this channel")
	}
	return tbl
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

	tableConfigStr := fmt.Sprintf("Table Config:\n - Owner: %s\n - Game: **%s**\n - Timeout: **%d**\n - Seats: **%d**\n - Limit: **%s**\n - Stakes (small, big, ante): **%d**, **%d**, **%d**\n",
		tbl.OwnerName, tbl.Table.Game().String(), tbl.GetTimeout(), tbl.Table.NumOfSeats(), tbl.Table.Limit(), stakes.SmallBet, stakes.BigBet, stakes.Ante)

	playersStr := ""

	for k, v := range tbl.Table.Players() {
		tablePlayer := v.Player().(*TablePlayer)
		playersStr += fmt.Sprintf("Seat [%d] %s: $%d\n", k, tablePlayer.Name, v.Chips())
	}

	go SurelySend(channel, tableConfigStr+"\n"+playersStr+"\n+You can change settings using conf set {setting} {value}")
}

func (t *TableManager) GetTable(channel string) *Table {
	for _, tbl := range t.tables {
		if tbl.Channel == channel {
			return tbl
		}
	}
	return nil
}
