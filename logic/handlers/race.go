package handlers

import (
	"math/rand"
	"srserver/common"
	"srserver/logic/clients"
	"srserver/logic/db"
	"srserver/logic/raceflow"
	"srserver/logic/races"
	"srserver/logic/utils"
	pb "srserver/proto/out"
)

// RaceManager is the single authority for the start -> finish/abandon
// lifecycle of every race on this server.
var RaceManager = raceflow.NewManager(utils.MakeId)

// Indirection seams so the packet handlers can be exercised in tests
// without the static content database (resources submodule).
var (
	findTrack   = db.FindTrack
	randomTrack = func() *pb.BaseTrack {
		baseTracks := db.Database.TrackDatabase.GetItems()
		return baseTracks[rand.Intn(len(baseTracks))]
	}
	getEnemyByID      = races.GetEnemyById
	createShadowEnemy = races.CreateEnemy
)

// CreateRace handles method 703. The resource occupation and the active-race
// record are created atomically inside RaceManager.Start; on any validation
// failure an error packet is returned and no balance/counter is changed.
func CreateRace(pack *common.Pack, client *clients.Client) {
	startParams := &pb.StartParams{}
	pack.ReadProto(startParams)
	player := client.Player
	response := common.NewResponse(pack)

	activeRace, err := RaceManager.Start(player, startParams.Type, startParams.EnemyId)
	if err != nil {
		response.SetError(true)
		client.Write(response.ToByteBuff())
		return
	}

	baseTrack := findTrack(2018)
	if activeRace.Type == pb.RaceType_RACE || baseTrack == nil {
		baseTrack = randomTrack()
	}
	track := utils.BuildTrack(baseTrack, player.User.World.ZoneId)
	track.Distanse = activeRace.Distance
	track.Distanse += 3.25 // Burnout zone distance

	var enemy *pb.Enemy
	switch activeRace.Type {
	case pb.RaceType_SHADOW:
		enemy = createShadowEnemy(player)
	case pb.RaceType_RACE, pb.RaceType_POINTS:
		enemy = getEnemyByID(activeRace.EnemyId)
	}
	if enemy != nil {
		enemy.Type = activeRace.Type
	}

	response.WriteProto(track)
	if enemy != nil {
		response.WriteProto(enemy)
	}
	client.Write(response.ToByteBuff())
}

// GetRaceAward handles method 704. A finish is honoured only when it matches
// the race recorded at CreateRace; the record is consumed before the reward
// is granted, so forged and repeated finishes pay nothing.
func GetRaceAward(pack *common.Pack, client *clients.Client) {
	finishParams := &pb.FinishParams{}
	pack.ReadProto(finishParams)
	player := client.Player
	response := common.NewResponse(pack)

	result, err := RaceManager.Finish(player, finishParams)
	if err != nil {
		response.SetError(true)
		client.Write(response.ToByteBuff())
		return
	}

	if car := player.GetCurrentCar(); car != nil {
		car.AccumulatorDistance += finishParams.UserDistance
	}

	race := &pb.Race{
		Money:    result.Money,
		Items:    result.Items,
		Result:   result.Result,
		Type:     result.Type,
		UserTime: finishParams.UserTime,
	}

	if result.EnemyId > 0 {
		if enemy := getEnemyByID(result.EnemyId); enemy != nil {
			enemy.EnemyType.RaceCount++
			enemy.EnemyType.Races = append(enemy.EnemyType.Races, result.Result)
			if enemy.EnemyType.RaceCount < 3 {
				race.IsCanRepeat = true
			}
		}
	}

	response.WriteProto(race)
	client.Write(response.ToByteBuff())
}

// BrakeRace handles method 705 ("brake"/abandon). The occupied fuel, entry
// fee or race-count slot is restored to its pre-start state. A brake packet
// without an active race no longer consumes fuel.
func BrakeRace(pack *common.Pack, client *clients.Client) {
	response := common.NewResponse(pack)
	if err := RaceManager.Abandon(client.Player); err != nil {
		response.SetError(true)
	}
	client.Write(response.ToByteBuff())
}

// OnClientDisconnect must be called when a client connection drops. Any
// in-progress race is restored exactly once and its record removed.
func OnClientDisconnect(client *clients.Client) {
	if client == nil || client.Player == nil {
		return
	}
	RaceManager.HandleDisconnect(client.Player)
}

func UpdateEnemies(pack *common.Pack, client *clients.Client) {
	enemies := &pb.UserEnemies{
		CarId: client.Player.User.Garage.CurrentId,
		List: []*pb.Enemy{
			races.CreateBotEnemy(),
			races.CreateBotEnemy(),
			//race.GetEnemyFor(client.Player),
			races.CreateBotEnemy(),
		},
	}
	client.Player.User.Enemies = enemies
	client.Player.Save()
	response := common.NewResponse(pack)
	response.WriteProto(enemies)
	client.Write(response.ToByteBuff())
}

func UpdatePointsEnemies(pack *common.Pack, client *clients.Client) {
	pointsEnemies := &pb.PointsEnemies{
		List: []*pb.Enemy{
			races.CreateBotEnemy(),
			races.CreateBotEnemy(),
		},
		IsNeedUpdate: false,
	}
	client.Player.User.PointsEnemies = pointsEnemies
	response := common.NewResponse(pack)
	response.WriteProto(pointsEnemies)
	client.Write(response.ToByteBuff())
}
