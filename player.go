package main

import (
	"encoding/json"
	"io/ioutil"
	"log"
	"os"
	"sync"
	"time"
)

type Player struct {
	sync.Mutex
	ID    string
	Name  string
	Money int
}

type PlayerManager struct {
	sync.RWMutex
	Players []*Player
	Stop    chan *sync.WaitGroup
}

func (pm *PlayerManager) Run() {
	err := pm.Load()
	if err != nil {
		log.Println("Failed loading data, consider using backup")
	}

	ticker := time.NewTicker(time.Minute)
	for {
		select {
		case <-ticker.C:
			err := pm.Save()
			if err != nil {
				log.Printf("Error saving log: ", err)
			}
		case wg := <-pm.Stop:
			pm.Save()
			wg.Done()
			return
		}
	}
}

func (pm *PlayerManager) Load() error {
	file, err := ioutil.ReadFile("players.json")
	if err != nil {
		return err
	}
	var decoded []*Player
	err = json.Unmarshal(file, &decoded)
	if err != nil {
		return err
	}

	pm.Lock()
	pm.Players = decoded
	pm.Unlock()
	return nil
}

func (pm *PlayerManager) Save() error {
	// Rotate savedata if existing
	_, err := os.Stat("players.json")
	if err == nil {

		err := os.Rename("players.json", "players.json.1")
		if err != nil {
			return err
		}
	}

	pm.Lock()
	out, err := json.Marshal(pm.Players)
	pm.Unlock()
	if err != nil {
		return err
	}

	file, err := os.Create("players.json")
	if err != nil {
		return err
	}
	file.Write(out)
	return file.Close()
}

func (pm *PlayerManager) AddPlayer(player *Player, lock bool) {
	if lock {
		pm.Lock()
		defer pm.Unlock()
	}

	pm.Players = append(pm.Players, player)
}

func (pm *PlayerManager) GetCreatePlayer(id, name string) *Player {
	pm.Lock()
	defer pm.Unlock()

	for _, v := range pm.Players {
		if v.ID == id {
			return v
		}
	}

	player := &Player{
		Name:  name,
		ID:    id,
		Money: 100,
	}
	pm.AddPlayer(player, false)
	return player
}
