package node

import (
	"fmt"
	"github.com/spacemeshos/go-spacemesh/address"
	"github.com/spacemeshos/go-spacemesh/amcl/BLS381"
	"github.com/spacemeshos/go-spacemesh/api"
	"github.com/spacemeshos/go-spacemesh/eligibility"
	"github.com/spacemeshos/go-spacemesh/log"
	"github.com/spacemeshos/go-spacemesh/miner"
	"github.com/spacemeshos/go-spacemesh/nipst"
	"github.com/spacemeshos/go-spacemesh/oracle"
	"github.com/spacemeshos/go-spacemesh/p2p/service"
	"github.com/spacemeshos/go-spacemesh/signing"
	"github.com/spacemeshos/go-spacemesh/types"
	"github.com/spacemeshos/poet/integration"
//	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

type AppTestSuite struct {
	suite.Suite

	apps        []*SpacemeshApp
	dbs         []string
	poetCleanup func() error
}

func (suite *AppTestSuite) SetupTest() {
	suite.apps = make([]*SpacemeshApp, 0, 0)
	suite.dbs = make([]string, 0, 0)
}

// NewRPCPoetHarnessClient returns a new instance of RPCPoetClient
// which utilizes a local self-contained poet server instance
// in order to exercise functionality.
func NewRPCPoetHarnessClient() (*nipst.RPCPoetClient, error) {
	cfg, err := integration.DefaultConfig()
	if err != nil {
		return nil, err
	}
	cfg.NodeAddress = "127.0.0.1:9091"
	cfg.InitialRoundDuration = time.Duration(35 * time.Second).String()

	h, err := integration.NewHarness(cfg)
	if err != nil {
		return nil, err
	}

	return nipst.NewRPCPoetClient(h.PoetClient, h.TearDown), nil
}

func (suite *AppTestSuite) TearDownTest() {
	if err := suite.poetCleanup(); err != nil {
		log.Error("error while cleaning up PoET: %v", err)
	}
	for _, dbinst := range suite.dbs {
		if err := os.RemoveAll(dbinst); err != nil {
			panic(fmt.Sprintf("what happened : %v", err))
		}
	}
	if err := os.RemoveAll("../tmp"); err != nil {
		log.Error("error while cleaning up tmp dir: %v", err)
	}
	//poet should clean up after himself
	if matches, err := filepath.Glob("*.bin"); err != nil {
		log.Error("error while finding PoET bin files: %v", err)
	} else {
		for _, f := range matches {
			if err = os.Remove(f); err != nil {
				log.Error("error while cleaning up PoET bin files: %v", err)
			}
		}
	}
}

func (suite *AppTestSuite) initMultipleInstances(numOfInstances int, storeFormat string) {
	r := require.New(suite.T())

	net := service.NewSimulator()
	runningName := 'a'
	rolacle := eligibility.New()
	poet, err := NewRPCPoetHarnessClient()
	r.NoError(err)
	suite.poetCleanup = poet.CleanUp
	rng := BLS381.DefaultSeed()
	for i := 0; i < numOfInstances; i++ {
		smApp := NewSpacemeshApp()
		smApp.Config.HARE.N = numOfInstances
		smApp.Config.HARE.F = numOfInstances / 2
		smApp.Config.HARE.ExpectedLeaders = 5
		smApp.Config.CoinbaseAccount = strconv.Itoa(i + 1)
		smApp.Config.HARE.RoundDuration = 30
		smApp.Config.HARE.WakeupDelta = 30
		smApp.Config.LayerAvgSize = numOfInstances
		smApp.Config.CONSENSUS.LayersPerEpoch = 4
		smApp.Config.LayerDurationSec = 180

		edSgn := signing.NewEdSigner()
		pub := edSgn.PublicKey()

		r.NoError(err)
		vrfPriv, vrfPub := BLS381.GenKeyPair(rng)
		vrfSigner := BLS381.NewBlsSigner(vrfPriv)
		nodeID := types.NodeId{Key: pub.String(), VRFPublicKey: vrfPub}

		swarm := net.NewNode()
		dbStorepath := storeFormat + string(runningName)

		hareOracle := oracle.NewLocalOracle(rolacle, numOfInstances, nodeID)
		hareOracle.Register(true, pub.String())

		layerSize := numOfInstances
		npstCfg := nipst.PostParams{
			Difficulty:           5,
			NumberOfProvenLabels: 10,
			SpaceUnit:            1024,
		}
		err = smApp.initServices(nodeID, swarm, dbStorepath, edSgn, false, hareOracle, uint32(layerSize), nipst.NewPostClient(), poet, vrfSigner, npstCfg, uint32(smApp.Config.CONSENSUS.LayersPerEpoch))
		r.NoError(err)
		smApp.setupGenesis()

		suite.apps = append(suite.apps, smApp)
		suite.dbs = append(suite.dbs, dbStorepath)
		runningName++
	}
	activateGrpcServer(suite.apps[0])
}

func activateGrpcServer(smApp *SpacemeshApp) {
	smApp.Config.API.StartGrpcServer = true
	smApp.grpcAPIService = api.NewGrpcService(smApp.P2P, smApp.state, smApp.txProcessor)
	smApp.grpcAPIService.StartService(nil)
}

func (suite *AppTestSuite) TestMultipleNodes() {
	//EntryPointCreated <- true

	const numberOfEpochs = 2 // first 2 epochs are genesis
	addr := address.BytesToAddress([]byte{0x01})
	dst := address.BytesToAddress([]byte{0x02})
	tx := types.SerializableTransaction{}
	tx.Amount = big.NewInt(10).Bytes()
	tx.GasLimit = 1
	tx.Origin = addr
	tx.Recipient = &dst
	tx.Price = big.NewInt(1).Bytes()

	txbytes, _ := types.TransactionAsBytes(&tx)
	path := "../tmp/test/state_" + time.Now().String()
	suite.initMultipleInstances(50, path)
	for _, a := range suite.apps {
		a.startServices()
	}

	defer suite.gracefulShutdown()

	_ = suite.apps[0].P2P.Broadcast(miner.IncomingTxProtocol, txbytes)
	timeout := time.After(60 * time.Second * 60)

	stickyClientsDone := 0
	loop:
	for {
		select {
		// Got a timeout! fail with a timeout error
		case <-timeout:
			suite.T().Fatal("timed out")
		default:
			maxClientsDone := 0
			for idx, app := range suite.apps {
				if big.NewInt(10).Cmp(app.state.GetBalance(dst)) == 0 &&
					uint32(suite.apps[idx].mesh.LatestLayer()) == numberOfEpochs*uint32(suite.apps[idx].Config.CONSENSUS.LayersPerEpoch)+1 { // make sure all had 1 non-genesis layer

					suite.validateLastATXActiveSetSize(app)
					clientsDone := 0
					for idx2, app2 := range suite.apps {
						if idx != idx2 {
							r1 := app.state.IntermediateRoot(false).String()
							r2 := app2.state.IntermediateRoot(false).String()
							if r1 == r2 {
								clientsDone++
								if clientsDone == len(suite.apps)-1 {
									log.Info("%d roots confirmed out of %d", clientsDone, len(suite.apps))
									break loop
								}
							}
						}
					}
					if clientsDone > maxClientsDone {
						maxClientsDone = clientsDone
					}
				}
			}
			if maxClientsDone != stickyClientsDone {
				stickyClientsDone = maxClientsDone
				log.Info("%d roots confirmed out of %d", maxClientsDone, len(suite.apps))
			}
			time.Sleep(30 * time.Second)
		}
	}

	for i := 3; true; i++ {
		suite.validateBlocksAndATXs(types.LayerID(i*4-1))
	}
}

func (suite *AppTestSuite) validateBlocksAndATXs(untilLayer types.LayerID) {

	type nodeData struct {
		layertoblocks map[types.LayerID][]types.BlockID
		atxPerEpoch   map[types.EpochId][]types.AtxId
	}

	datamap := make(map[string]*nodeData)

	// wait until all nodes are in `untilLayer`
	for {

		count := 0
		for _, ap := range suite.apps {
			curNodeLastLayer := ap.blockListener.ValidatedLayer()
			if curNodeLastLayer < untilLayer {
				//log.Info("layer for %v was %v, want %v", ap.nodeId.Key, curNodeLastLayer, 8)
			} else {
				count++
			}
		}

		if count == len(suite.apps) {
			break
		}

		time.Sleep(30 * time.Second)
	}

	for _, ap := range suite.apps {
		if _, ok := datamap[ap.nodeId.Key]; !ok {
			datamap[ap.nodeId.Key] = new(nodeData)
			datamap[ap.nodeId.Key].atxPerEpoch = make(map[types.EpochId][]types.AtxId)
			datamap[ap.nodeId.Key].layertoblocks = make(map[types.LayerID][]types.BlockID)
		}

		for i := types.LayerID(0); i <= untilLayer; i++ {
			lyr, err := ap.blockListener.GetLayer(i)
			if err != nil {
				log.Error("ERRORRROROROR ", err)
			}
			for _, b := range lyr.Blocks() {
				datamap[ap.nodeId.Key].layertoblocks[lyr.Index()] = append(datamap[ap.nodeId.Key].layertoblocks[lyr.Index()], b.ID())
			}
			epoch := lyr.Index().GetEpoch(ap.Config.CONSENSUS.LayersPerEpoch)
			if _, ok := datamap[ap.nodeId.Key].atxPerEpoch[epoch]; !ok {
				atxs, err := ap.blockListener.AtxDB.GetEpochAtxIds(epoch)
				if err != nil {
					log.Error("EERRRO RWTWTFFF FFFF ")
				}
				datamap[ap.nodeId.Key].atxPerEpoch[epoch] = atxs
			}
		}
	}

	for i, d := range datamap {
		log.Info("Node %v in len(layerstoblocks) %v", i, len(d.layertoblocks))
		for i2, d2 := range datamap {
			if i == i2 {
				continue
			}
			if len(d.layertoblocks) != len(d2.layertoblocks) {
			fmt.Printf("%v has not matching layer to %v. %v not %v \r\n", i, i2, len(d.layertoblocks), len(d2.layertoblocks))
			panic("WTF")
		}

			for l, bl := range d.layertoblocks {
				 if len(bl) != len(d2.layertoblocks[l]) {
					fmt.Println(fmt.Sprintf("%v and %v had different block maps for layer: %v: %v: %v \r\n %v: %v \r\n\r\n", i, i2, l, i, bl, i2, d2.layertoblocks[l]))
				panic("WTF")
				}
			}

			for e, atx := range d.atxPerEpoch {
				if len(atx) != len(d2.atxPerEpoch[e]) {
					fmt.Printf("%v and %v had different atx maps for epoch: %v: %v: %v \r\n %v: %v \r\n\r\n", i, i2, e, i, atx, i2, d2.atxPerEpoch[e])
panic("WTFFFFF")
					}
			}
		}
	}

	// assuming all nodes have the same results
	layers_per_epoch := int(suite.apps[0].Config.CONSENSUS.LayersPerEpoch)
	layer_avg_size := suite.apps[0].Config.LayerAvgSize
	patient := datamap[suite.apps[0].nodeId.Key]

	lastlayer := len(patient.layertoblocks)

	total_blocks := 0
	for _, l := range patient.layertoblocks {
		total_blocks += len(l)
	}

	first_epoch_blocks := 0

	for i := 0; i < layers_per_epoch; i++ {
		if l, ok := patient.layertoblocks[types.LayerID(i)]; ok {
			first_epoch_blocks += len(l)
		}
	}

	total_atxs := 0

	for _, atxs := range patient.atxPerEpoch {
		total_atxs += len(atxs)
	}

	if ((total_blocks-first_epoch_blocks)/(lastlayer-layers_per_epoch)) != layer_avg_size {
		fmt.Printf("not good num of blocks got: %v, want: %v. total_blocks: %v, first_epoch_blocks: %v, lastlayer: %v, layers_per_epoch: %v \r\n\r\n\r\n", (total_blocks-first_epoch_blocks)/(lastlayer-layers_per_epoch), layer_avg_size, total_blocks, first_epoch_blocks, lastlayer, layers_per_epoch)
panic("NOT ENOUGH BLOCKS")
}
	if total_atxs != (lastlayer/layers_per_epoch)*len(suite.apps) {
		fmt.Printf("not good num of atxs got: %v, want: %v\r\n", total_atxs, (lastlayer/layers_per_epoch)*len(suite.apps))
		panic("NOT ENOUGH ATX")

	}

	fmt.Printf("ALL ASSERTS FOR %v WAS SUCCESSFULL ##################################################################\r\n", untilLayer)
}

func (suite *AppTestSuite) validateLastATXActiveSetSize(app *SpacemeshApp) {
	prevAtxId, err := app.atxBuilder.GetPrevAtxId(app.nodeId)
	suite.NoError(err)
	atx, err := app.mesh.GetAtx(*prevAtxId)
	suite.NoError(err)
	suite.Equal(len(suite.apps), int(atx.ActiveSetSize), "atx: %v node: %v", atx.ShortId(), app.nodeId.Key[:5])
}

func (suite *AppTestSuite) gracefulShutdown() {
	var wg sync.WaitGroup
	for _, app := range suite.apps {
		go func(app *SpacemeshApp) {
			wg.Add(1)
			app.stopServices()
			wg.Done()
		}(app)
	}
	wg.Wait()
}

func TestAppTestSuite(t *testing.T) {
	//defer leaktest.Check(t)()
	suite.Run(t, new(AppTestSuite))
}
