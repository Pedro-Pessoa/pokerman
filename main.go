package main

import (
	"flag"
	"github.com/bwmarrin/discordgo"
	"github.com/jonas747/dutil/commandsystem"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"
)

const (
	VERSION = "PokerMan 0.5 Alpha"
)

var (
	flagToken string
	flagDebug bool

	dgo       *discordgo.Session
	cmdSystem *commandsystem.System

	tableManager = &TableManager{
		tables:  make([]*Table, 0),
		EvtChan: make(chan interface{}),
	}
	playerManager = &PlayerManager{
		Players: make([]*Player, 0),
		Stop:    make(chan *sync.WaitGroup),
	}
)

func init() {
	flag.StringVar(&flagToken, "t", "", "Token to use")
	flag.BoolVar(&flagDebug, "d", false, "Set to turn on debug info, such as pprof http server")

	if !flag.Parsed() {
		flag.Parse()
	}
}

func PanicErr(err error) {
	if err != nil {
		panic(err)
	}
}

func main() {
	log.Println("Launching " + VERSION)

	session, err := discordgo.New(flagToken)
	PanicErr(err)

	cmdSystem = &commandsystem.System{
		Session: session,
	}
	cmdSystem.RegisterCommands(Commands...)

	session.AddHandler(cmdSystem.HandleMessageCreate)
	session.AddHandler(HandleMessageCreate)
	session.AddHandler(HandleReady)
	session.AddHandler(HandleServerJoin)

	err = session.Open()
	PanicErr(err)

	dgo = session

	go tableManager.Run()
	go playerManager.Run()

	signalChan := make(chan os.Signal)
	go HandleSignal(signalChan)
	signal.Notify(signalChan, os.Kill, os.Interrupt)

	select {}
}

func HandleSignal(stopchan chan os.Signal) {
	<-stopchan
	var wg sync.WaitGroup

	// Stop tables first
	log.Println("\nWaiting for tablemanager to finish")
	wg.Add(1)
	tableManager.EvtChan <- &StopEvt{wg: &wg}
	wg.Wait()

	// Sleep for a second to allow modifying moneis
	time.Sleep(time.Second)

	// Then save players
	log.Println("Waiting for playermanager to finish")
	wg.Add(1)
	playerManager.Stop <- &wg
	wg.Wait()
	log.Println("Sucessfully shut down")
	os.Exit(0)
}

func HandleReady(s *discordgo.Session, r *discordgo.Ready) {
	log.Println("Ready received! Connected to", len(s.State.Guilds), "Guilds")
}

func HandleServerJoin(s *discordgo.Session, g *discordgo.GuildCreate) {
	log.Println("Joined guild", g.Name, " Connected to", len(s.State.Guilds), "Guilds")
}

func HandleMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	action := GetAction(m.Content)
	if action != nil {
		log.Println("Got action mon")
		// An action lets pass it to tablemanager
		tableManager.EvtChan <- &ActionEvt{Action: action, Channel: m.ChannelID, PlayerID: m.Author.ID}
	}
}

// Will retry up to 5 times on 502's
func SurelySend(channel, msg string) {
	sendDiscordMessageRetry(channel, msg, 0, 5)
}

func sendDiscordMessageRetry(channel, msg string, counter int, max int) {
	_, err := dgo.ChannelMessageSend(channel, msg)
	if err != nil {
		if strings.Contains(err.Error(), "502") {
			if counter < max {
				log.Println("Retrying sending message")
				sendDiscordMessageRetry(channel, msg, counter+1, max)
			} else {
				log.Println("No more retries left :(")
			}
		} else {
			log.Println("Failed sending message:", err)
		}
	}
}
