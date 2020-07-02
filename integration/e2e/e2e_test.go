/*
Copyright IBM Corp All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	docker "github.com/fsouza/go-dockerclient"
	"github.com/golang/protobuf/proto"
	"github.com/hyperledger/fabric-lib-go/healthz"
	"github.com/hyperledger/fabric-protos-go/orderer/etcdraft"
	"github.com/hyperledger/fabric/integration/nwo"
	"github.com/hyperledger/fabric/integration/nwo/commands"
	"github.com/hyperledger/fabric/integration/nwo/fabricconfig"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
	"github.com/onsi/gomega/gexec"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/ginkgomon"
)

var _ = Describe("EndToEnd", func() {
	var (
		testDir   string
		client    *docker.Client
		network   *nwo.Network
		chaincode nwo.Chaincode
		process   ifrit.Process
	)

	BeforeEach(func() {
		var err error
		testDir, err = ioutil.TempDir("", "e2e")
		Expect(err).NotTo(HaveOccurred())

		client, err = docker.NewClientFromEnv()
		Expect(err).NotTo(HaveOccurred())

		chaincode = nwo.Chaincode{
			Name:            "mycc",
			Version:         "0.0",
			Path:            components.Build("github.com/hyperledger/fabric/integration/chaincode/simple/cmd"),
			Lang:            "binary",
			PackageFile:     filepath.Join(testDir, "simplecc.tar.gz"),
			Ctor:            `{"Args":["init","a","100","b","200"]}`,
			SignaturePolicy: `AND ('Org1MSP.member','Org2MSP.member')`,
			Sequence:        "1",
			InitRequired:    true,
			Label:           "my_prebuilt_chaincode",
		}
	})

	AfterEach(func() {
		if process != nil {
			process.Signal(syscall.SIGTERM)
			Eventually(process.Wait(), network.EventuallyTimeout).Should(Receive())
		}
		if network != nil {
			network.Cleanup()
		}
		os.RemoveAll(testDir)
	})

	Describe("basic solo network with 2 orgs and no docker", func() {
		var (
			metricsReader        *MetricsReader
			runArtifactsFilePath string
		)

		BeforeEach(func() {
			metricsReader = NewMetricsReader()
			go metricsReader.Start()

			network = nwo.New(nwo.BasicSolo(), testDir, nil, StartPort(), components)
			network.MetricsProvider = "statsd"
			network.StatsdEndpoint = metricsReader.Address()
			network.ChannelParticipationEnabled = true
			network.Profiles = append(network.Profiles, &nwo.Profile{
				Name:          "TwoOrgsBaseProfileChannel",
				Consortium:    "SampleConsortium",
				Orderers:      []string{"orderer"},
				Organizations: []string{"Org1", "Org2"},
			})
			network.Channels = append(network.Channels, &nwo.Channel{
				Name:        "baseprofilechannel",
				Profile:     "TwoOrgsBaseProfileChannel",
				BaseProfile: "TwoOrgsOrdererGenesis",
			})

			runArtifactsFilePath = filepath.Join(testDir, "run-artifacts.txt")
			os.Setenv("RUN_ARTIFACTS_FILE", runArtifactsFilePath)
			for i, e := range network.ExternalBuilders {
				e.PropagateEnvironment = append(e.PropagateEnvironment, "RUN_ARTIFACTS_FILE")
				network.ExternalBuilders[i] = e
			}

			network.GenerateConfigTree()
			for _, peer := range network.PeersWithChannel("testchannel") {
				core := network.ReadPeerConfig(peer)
				core.VM = nil
				network.WritePeerConfig(peer, core)
			}
			network.Bootstrap()

			networkRunner := network.NetworkGroupRunner()
			process = ifrit.Invoke(networkRunner)
			Eventually(process.Ready(), network.EventuallyTimeout).Should(BeClosed())
		})

		AfterEach(func() {
			if metricsReader != nil {
				metricsReader.Close()
			}

			// Terminate the processes but defer the network cleanup to the outer
			// AfterEach.
			if process != nil {
				process.Signal(syscall.SIGTERM)
				Eventually(process.Wait(), network.EventuallyTimeout).Should(Receive())
				process = nil
			}

			// Ensure that the temporary directories generated by launched external
			// chaincodes have been cleaned up. This must be done after the peers
			// have been terminated.
			contents, err := ioutil.ReadFile(runArtifactsFilePath)
			Expect(err).NotTo(HaveOccurred())
			scanner := bufio.NewScanner(bytes.NewBuffer(contents))
			for scanner.Scan() {
				Expect(scanner.Text()).NotTo(BeAnExistingFile())
			}
		})

		It("executes a basic solo network with 2 orgs and no docker", func() {
			By("getting the orderer by name")
			orderer := network.Orderer("orderer")

			By("setting up the channel")
			network.CreateAndJoinChannel(orderer, "testchannel")
			nwo.EnableCapabilities(network, "testchannel", "Application", "V2_0", orderer, network.Peer("Org1", "peer0"), network.Peer("Org2", "peer0"))

			By("attempting to install unsupported chaincode without docker")
			badCC := chaincode
			badCC.Lang = "unsupported-type"
			badCC.Label = "chaincode-label"
			badCC.PackageFile = filepath.Join(testDir, "unsupported-type.tar.gz")
			nwo.PackageChaincodeBinary(badCC)
			badCC.SetPackageIDFromPackageFile()
			sess, err := network.PeerAdminSession(
				network.Peer("Org1", "peer0"),
				commands.ChaincodeInstall{
					PackageFile: badCC.PackageFile,
					ClientAuth:  network.ClientAuthRequired,
				},
			)
			Expect(err).NotTo(HaveOccurred())
			Eventually(sess, network.EventuallyTimeout).Should(gexec.Exit(1))
			Expect(sess.Err).To(gbytes.Say("docker build is disabled"))

			By("deploying the chaincode")
			nwo.DeployChaincode(network, "testchannel", orderer, chaincode)

			By("ensuring external cc run artifacts exist after deploying")
			contents, err := ioutil.ReadFile(runArtifactsFilePath)
			Expect(err).NotTo(HaveOccurred())
			scanner := bufio.NewScanner(bytes.NewBuffer(contents))
			for scanner.Scan() {
				Expect(scanner.Text()).To(BeADirectory())
			}

			By("getting the client peer by name")
			peer := network.Peer("Org1", "peer0")

			RunQueryInvokeQuery(network, orderer, peer, "testchannel")
			RunRespondWith(network, orderer, peer, "testchannel")

			By("evaluating statsd metrics")
			metricsWriteInterval := 5 * time.Second
			CheckPeerStatsdStreamMetrics(metricsReader, 2*metricsWriteInterval)
			CheckPeerStatsdMetrics("org1_peer0", metricsReader, 2*metricsWriteInterval)
			CheckPeerStatsdMetrics("org2_peer0", metricsReader, 2*metricsWriteInterval)

			By("checking for orderer metrics")
			CheckOrdererStatsdMetrics("ordererorg_orderer", metricsReader, 2*metricsWriteInterval)

			By("setting up a channel from a base profile")
			additionalPeer := network.Peer("Org2", "peer0")
			network.CreateChannel("baseprofilechannel", orderer, peer, additionalPeer)
		})
	})

	Describe("basic kafka network with 2 orgs", func() {
		BeforeEach(func() {
			network = nwo.New(nwo.BasicKafka(), testDir, client, StartPort(), components)
			network.MetricsProvider = "prometheus"
			network.ChannelParticipationEnabled = true
			network.GenerateConfigTree()
			network.Bootstrap()

			networkRunner := network.NetworkGroupRunner()
			process = ifrit.Invoke(networkRunner)
			Eventually(process.Ready(), network.EventuallyTimeout).Should(BeClosed())
		})

		It("executes a basic kafka network with 2 orgs (using docker chaincode builds)", func() {
			chaincodePath, err := filepath.Abs("../chaincode/module")
			Expect(err).NotTo(HaveOccurred())

			// use these two variants of the same chaincode to ensure we test
			// the golang docker build for both module and gopath chaincode
			chaincode = nwo.Chaincode{
				Name:            "mycc",
				Version:         "0.0",
				Path:            chaincodePath,
				Lang:            "golang",
				PackageFile:     filepath.Join(testDir, "modulecc.tar.gz"),
				Ctor:            `{"Args":["init","a","100","b","200"]}`,
				SignaturePolicy: `AND ('Org1MSP.member','Org2MSP.member')`,
				Sequence:        "1",
				InitRequired:    true,
				Label:           "my_module_chaincode",
			}

			gopathChaincode := nwo.Chaincode{
				Name:            "mycc",
				Version:         "0.0",
				Path:            "github.com/hyperledger/fabric/integration/chaincode/simple/cmd",
				Lang:            "golang",
				PackageFile:     filepath.Join(testDir, "simplecc.tar.gz"),
				Ctor:            `{"Args":["init","a","100","b","200"]}`,
				SignaturePolicy: `AND ('Org1MSP.member','Org2MSP.member')`,
				Sequence:        "1",
				InitRequired:    true,
				Label:           "my_simple_chaincode",
			}

			orderer := network.Orderer("orderer")

			network.CreateAndJoinChannel(orderer, "testchannel")
			nwo.EnableCapabilities(network, "testchannel", "Application", "V2_0", orderer, network.Peer("Org1", "peer0"), network.Peer("Org2", "peer0"))

			// package, install, and approve by org1 - module chaincode
			packageInstallApproveChaincode(network, "testchannel", orderer, chaincode, network.Peer("Org1", "peer0"))

			// package, install, and approve by org2 - gopath chaincode, same logic
			packageInstallApproveChaincode(network, "testchannel", orderer, gopathChaincode, network.Peer("Org2", "peer0"))

			testPeers := network.PeersWithChannel("testchannel")
			nwo.CheckCommitReadinessUntilReady(network, "testchannel", chaincode, network.PeerOrgs(), testPeers...)
			nwo.CommitChaincode(network, "testchannel", orderer, chaincode, testPeers[0], testPeers...)
			nwo.InitChaincode(network, "testchannel", orderer, chaincode, testPeers...)

			By("listing the containers after committing the chaincode definition")
			initialContainerFilter := map[string][]string{
				"name": {
					chaincodeContainerNameFilter(network, chaincode),
					chaincodeContainerNameFilter(network, gopathChaincode),
				},
			}

			containers, err := client.ListContainers(docker.ListContainersOptions{Filters: initialContainerFilter})
			Expect(err).NotTo(HaveOccurred())
			Expect(containers).To(HaveLen(2))

			RunQueryInvokeQuery(network, orderer, network.Peer("Org1", "peer0"), "testchannel")

			CheckPeerOperationEndpoints(network, network.Peer("Org2", "peer0"))
			CheckOrdererOperationEndpoints(network, orderer)

			// upgrade chaincode to v2.0 with different label
			chaincode.Version = "1.0"
			chaincode.Sequence = "2"
			chaincode.Label = "my_module_chaincode_updated"
			gopathChaincode.Version = "1.0"
			gopathChaincode.Sequence = "2"
			gopathChaincode.Label = "my_simple_chaincode_updated"

			// package, install, and approve by org1 - module chaincode
			packageInstallApproveChaincode(network, "testchannel", orderer, chaincode, network.Peer("Org1", "peer0"))

			// package, install, and approve by org2 - gopath chaincode, same logic
			packageInstallApproveChaincode(network, "testchannel", orderer, gopathChaincode, network.Peer("Org2", "peer0"))

			nwo.CheckCommitReadinessUntilReady(network, "testchannel", chaincode, network.PeerOrgs(), testPeers...)
			nwo.CommitChaincode(network, "testchannel", orderer, chaincode, testPeers[0], testPeers...)
			nwo.InitChaincode(network, "testchannel", orderer, chaincode, testPeers...)

			By("listing the containers after updating the chaincode definition")
			// expect the containers for the previous package id to be stopped
			containers, err = client.ListContainers(docker.ListContainersOptions{Filters: initialContainerFilter})
			Expect(err).NotTo(HaveOccurred())
			Expect(containers).To(HaveLen(0))
			updatedContainerFilter := map[string][]string{
				"name": {
					chaincodeContainerNameFilter(network, chaincode),
					chaincodeContainerNameFilter(network, gopathChaincode),
				},
			}
			containers, err = client.ListContainers(docker.ListContainersOptions{Filters: updatedContainerFilter})
			Expect(err).NotTo(HaveOccurred())
			Expect(containers).To(HaveLen(2))

			RunQueryInvokeQuery(network, orderer, network.Peer("Org1", "peer0"), "testchannel")

			By("retrieving the local mspid of the peer via simple chaincode")
			sess, err := network.PeerUserSession(network.Peer("Org2", "peer0"), "User1", commands.ChaincodeQuery{
				ChannelID: "testchannel",
				Name:      "mycc",
				Ctor:      `{"Args":["mspid"]}`,
			})
			Expect(err).NotTo(HaveOccurred())
			Eventually(sess, network.EventuallyTimeout).Should(gexec.Exit(0))
			Expect(sess).To(gbytes.Say("Org2MSP"))
		})
	})

	Describe("basic single node etcdraft network", func() {
		var (
			peerRunners    []*ginkgomon.Runner
			processes      map[string]ifrit.Process
			ordererProcess ifrit.Process
		)

		BeforeEach(func() {
			network = nwo.New(nwo.MultiChannelEtcdRaft(), testDir, client, StartPort(), components)
			network.ChannelParticipationEnabled = true
			network.GenerateConfigTree()
			for _, peer := range network.Peers {
				core := network.ReadPeerConfig(peer)
				core.Peer.Gossip.UseLeaderElection = false
				core.Peer.Gossip.OrgLeader = true
				core.Peer.Deliveryclient.ReconnectTotalTimeThreshold = time.Duration(time.Second)
				network.WritePeerConfig(peer, core)
			}
			network.Bootstrap()

			ordererRunner := network.OrdererGroupRunner()
			ordererProcess = ifrit.Invoke(ordererRunner)
			Eventually(ordererProcess.Ready(), network.EventuallyTimeout).Should(BeClosed())

			peerRunners = make([]*ginkgomon.Runner, len(network.Peers))
			processes = map[string]ifrit.Process{}
			for i, peer := range network.Peers {
				pr := network.PeerRunner(peer)
				peerRunners[i] = pr
				p := ifrit.Invoke(pr)
				processes[peer.ID()] = p
				Eventually(p.Ready(), network.EventuallyTimeout).Should(BeClosed())
			}
		})

		AfterEach(func() {
			if ordererProcess != nil {
				ordererProcess.Signal(syscall.SIGTERM)
				Eventually(ordererProcess.Wait(), network.EventuallyTimeout).Should(Receive())
			}
			for _, p := range processes {
				p.Signal(syscall.SIGTERM)
				Eventually(p.Wait(), network.EventuallyTimeout).Should(Receive())
			}
		})

		It("creates two channels with two orgs trying to reconfigure and update metadata", func() {
			orderer := network.Orderer("orderer")
			peer := network.Peer("Org1", "peer0")

			By("Create first channel and deploy the chaincode")
			network.CreateAndJoinChannel(orderer, "testchannel")
			nwo.EnableCapabilities(network, "testchannel", "Application", "V2_0", orderer, network.Peer("Org1", "peer0"), network.Peer("Org2", "peer0"))
			nwo.DeployChaincode(network, "testchannel", orderer, chaincode)
			RunQueryInvokeQuery(network, orderer, peer, "testchannel")

			By("Create second channel and deploy chaincode")
			network.CreateAndJoinChannel(orderer, "testchannel2")
			peers := network.PeersWithChannel("testchannel2")
			nwo.EnableCapabilities(network, "testchannel2", "Application", "V2_0", orderer, network.Peer("Org1", "peer0"), network.Peer("Org2", "peer0"))
			nwo.ApproveChaincodeForMyOrg(network, "testchannel2", orderer, chaincode, peers...)
			nwo.CheckCommitReadinessUntilReady(network, "testchannel2", chaincode, network.PeerOrgs(), peers...)
			nwo.CommitChaincode(network, "testchannel2", orderer, chaincode, peers[0], peers...)
			nwo.InitChaincode(network, "testchannel2", orderer, chaincode, peers...)
			RunQueryInvokeQuery(network, orderer, peer, "testchannel2")

			By("Update consensus metadata to increase snapshot interval")
			snapDir := path.Join(network.RootDir, "orderers", orderer.ID(), "etcdraft", "snapshot", "testchannel")
			files, err := ioutil.ReadDir(snapDir)
			Expect(err).NotTo(HaveOccurred())
			numOfSnaps := len(files)

			nwo.UpdateConsensusMetadata(network, peer, orderer, "testchannel", func(originalMetadata []byte) []byte {
				metadata := &etcdraft.ConfigMetadata{}
				err := proto.Unmarshal(originalMetadata, metadata)
				Expect(err).NotTo(HaveOccurred())

				// update max in flight messages
				metadata.Options.MaxInflightBlocks = 1000
				metadata.Options.SnapshotIntervalSize = 10 * 1024 * 1024 // 10 MB

				// write metadata back
				newMetadata, err := proto.Marshal(metadata)
				Expect(err).NotTo(HaveOccurred())
				return newMetadata
			})

			// assert that no new snapshot is taken because SnapshotIntervalSize has just enlarged
			files, err = ioutil.ReadDir(snapDir)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(files)).To(Equal(numOfSnaps))

			By("ensuring that static leaders do not give up on retrieving blocks after the orderer goes down")
			ordererProcess.Signal(syscall.SIGTERM)
			Eventually(ordererProcess.Wait(), network.EventuallyTimeout).Should(Receive())
			for _, peerRunner := range peerRunners {
				Eventually(peerRunner.Err(), network.EventuallyTimeout).Should(gbytes.Say("peer is a static leader, ignoring peer.deliveryclient.reconnectTotalTimeThreshold"))
			}
		})
	})

	Describe("single node etcdraft network with remapped orderer endpoints", func() {
		BeforeEach(func() {
			network = nwo.New(nwo.MinimalRaft(), testDir, client, StartPort(), components)
			network.GenerateConfigTree()

			configtxConfig := network.ReadConfigTxConfig()
			ordererEndpoints := configtxConfig.Profiles["SampleDevModeEtcdRaft"].Orderer.Organizations[0].OrdererEndpoints
			correctOrdererEndpoint := ordererEndpoints[0]
			ordererEndpoints[0] = "127.0.0.1:1"
			network.WriteConfigTxConfig(configtxConfig)

			peer := network.Peer("Org1", "peer0")
			peerConfig := network.ReadPeerConfig(peer)
			peerConfig.Peer.Deliveryclient.AddressOverrides = []*fabricconfig.AddressOverride{
				{
					From:        "127.0.0.1:1",
					To:          correctOrdererEndpoint,
					CACertsFile: network.CACertsBundlePath(),
				},
			}
			network.WritePeerConfig(peer, peerConfig)

			network.Bootstrap()

			networkRunner := network.NetworkGroupRunner()
			process = ifrit.Invoke(networkRunner)
			Eventually(process.Ready(), network.EventuallyTimeout).Should(BeClosed())
		})

		It("creates and updates channel", func() {
			orderer := network.Orderer("orderer")

			network.CreateAndJoinChannel(orderer, "testchannel")

			// The below call waits for the config update to commit on the peer, so
			// it will fail if the orderer addresses are wrong.
			nwo.EnableCapabilities(network, "testchannel", "Application", "V2_0", orderer, network.Peer("Org1", "peer0"), network.Peer("Org2", "peer0"))
		})
	})

	Describe("basic etcdraft network without a system channel", func() {
		var ordererProcess ifrit.Process
		BeforeEach(func() {
			raftConfig := nwo.BasicEtcdRaft()
			network = nwo.New(raftConfig, testDir, client, StartPort(), components)
			network.ChannelParticipationEnabled = true
			network.GenerateConfigTree()

			orderer := network.Orderer("orderer")
			ordererConfig := network.ReadOrdererConfig(orderer)
			ordererConfig.General.BootstrapMethod = "none"
			network.WriteOrdererConfig(orderer, ordererConfig)
			network.Bootstrap()

			ordererRunner := network.OrdererRunner(orderer)
			ordererProcess = ifrit.Invoke(ordererRunner)
			Eventually(ordererProcess.Ready, network.EventuallyTimeout).Should(BeClosed())
			Eventually(ordererRunner.Err(), network.EventuallyTimeout).Should(gbytes.Say("Registrar initializing without a system channel, number of application channels: 0"))
		})

		AfterEach(func() {
			if ordererProcess != nil {
				ordererProcess.Signal(syscall.SIGTERM)
				Eventually(ordererProcess.Wait(), network.EventuallyTimeout).Should(Receive())
			}
		})

		It("starts the orderer but rejects channel creation requests", func() {
			By("attempting to create a channel without a system channel defined")
			sess, err := network.PeerAdminSession(network.Peer("Org1", "peer0"), commands.ChannelCreate{
				ChannelID:   "testchannel",
				Orderer:     network.OrdererAddress(network.Orderer("orderer"), nwo.ListenPort),
				File:        network.CreateChannelTxPath("testchannel"),
				OutputBlock: "/dev/null",
				ClientAuth:  network.ClientAuthRequired,
			})
			Expect(err).NotTo(HaveOccurred())
			Eventually(sess, network.EventuallyTimeout).Should(gexec.Exit(1))
			Eventually(sess.Err, network.EventuallyTimeout).Should(gbytes.Say("channel creation request not allowed because the orderer system channel is not defined"))
		})
	})

	Describe("basic solo network with containers being interrupted", func() {
		BeforeEach(func() {
			network = nwo.New(nwo.FullSolo(), testDir, client, StartPort(), components)

			network.GenerateConfigTree()
			network.Bootstrap()

			networkRunner := network.NetworkGroupRunner()
			process = ifrit.Invoke(networkRunner)
			Eventually(process.Ready(), network.EventuallyTimeout).Should(BeClosed())
		})

		It("recreates terminated chaincode containers", func() {
			chaincode := nwo.Chaincode{
				Name:            "mycc",
				Version:         "0.0",
				Path:            "github.com/hyperledger/fabric/integration/chaincode/simple/cmd",
				Lang:            "golang",
				PackageFile:     filepath.Join(testDir, "simplecc.tar.gz"),
				Ctor:            `{"Args":["init","a","100","b","200"]}`,
				SignaturePolicy: `OR ('Org1MSP.peer', 'Org2MSP.peer')`,
				Sequence:        "1",
				InitRequired:    true,
				Label:           "my_simple_chaincode",
			}

			peer := network.Peers[0]
			orderer := network.Orderer("orderer")

			By("creating and joining channels")
			network.CreateAndJoinChannels(orderer)

			By("enabling new lifecycle capabilities")
			nwo.EnableCapabilities(network, "testchannel", "Application", "V2_0", orderer, network.Peer("Org1", "peer0"), network.Peer("Org2", "peer0"))
			By("deploying the chaincode")
			nwo.DeployChaincode(network, "testchannel", orderer, chaincode)

			By("querying and invoking chaincode")
			RunQueryInvokeQuery(network, orderer, peer, "testchannel")

			By("removing chaincode containers from all peers")
			listChaincodeContainers := docker.ListContainersOptions{
				Filters: map[string][]string{
					"name": {chaincodeContainerNameFilter(network, chaincode)},
				},
			}
			ctx := context.Background()
			containers, err := client.ListContainers(listChaincodeContainers)
			Expect(err).NotTo(HaveOccurred())
			Expect(containers).NotTo(BeEmpty())

			var originalContainerIDs []string
			for _, container := range containers {
				originalContainerIDs = append(originalContainerIDs, container.ID)
				err = client.RemoveContainer(docker.RemoveContainerOptions{
					ID:            container.ID,
					RemoveVolumes: true,
					Force:         true,
					Context:       ctx,
				})
				Expect(err).NotTo(HaveOccurred())
			}

			By("invoking chaincode against all peers in test channel")
			for _, peer := range network.Peers {
				sess, err := network.PeerUserSession(peer, "User1", commands.ChaincodeInvoke{
					ChannelID: "testchannel",
					Orderer:   network.OrdererAddress(orderer, nwo.ListenPort),
					Name:      "mycc",
					Ctor:      `{"Args":["invoke","a","b","10"]}`,
					PeerAddresses: []string{
						network.PeerAddress(peer, nwo.ListenPort),
					},
					WaitForEvent: true,
				})
				Expect(err).NotTo(HaveOccurred())
				Eventually(sess, network.EventuallyTimeout).Should(gexec.Exit(0))
				Expect(sess.Err).To(gbytes.Say("Chaincode invoke successful. result: status:200"))
			}

			By("checking successful removals of all old chaincode containers")
			newContainers, err := client.ListContainers(listChaincodeContainers)
			Expect(err).NotTo(HaveOccurred())
			Expect(newContainers).To(HaveLen(len(containers)))

			for _, container := range newContainers {
				Expect(originalContainerIDs).NotTo(ContainElement(container.ID))
			}
		})
	})
})

func RunQueryInvokeQuery(n *nwo.Network, orderer *nwo.Orderer, peer *nwo.Peer, channel string) {
	By("querying the chaincode")
	sess, err := n.PeerUserSession(peer, "User1", commands.ChaincodeQuery{
		ChannelID: channel,
		Name:      "mycc",
		Ctor:      `{"Args":["query","a"]}`,
	})
	Expect(err).NotTo(HaveOccurred())
	Eventually(sess, n.EventuallyTimeout).Should(gexec.Exit(0))
	Expect(sess).To(gbytes.Say("100"))

	sess, err = n.PeerUserSession(peer, "User1", commands.ChaincodeInvoke{
		ChannelID: channel,
		Orderer:   n.OrdererAddress(orderer, nwo.ListenPort),
		Name:      "mycc",
		Ctor:      `{"Args":["invoke","a","b","10"]}`,
		PeerAddresses: []string{
			n.PeerAddress(n.Peer("Org1", "peer0"), nwo.ListenPort),
			n.PeerAddress(n.Peer("Org2", "peer0"), nwo.ListenPort),
		},
		WaitForEvent: true,
	})
	Expect(err).NotTo(HaveOccurred())
	Eventually(sess, n.EventuallyTimeout).Should(gexec.Exit(0))
	Expect(sess.Err).To(gbytes.Say("Chaincode invoke successful. result: status:200"))

	sess, err = n.PeerUserSession(peer, "User1", commands.ChaincodeQuery{
		ChannelID: channel,
		Name:      "mycc",
		Ctor:      `{"Args":["query","a"]}`,
	})
	Expect(err).NotTo(HaveOccurred())
	Eventually(sess, n.EventuallyTimeout).Should(gexec.Exit(0))
	Expect(sess).To(gbytes.Say("90"))
}

func RunRespondWith(n *nwo.Network, orderer *nwo.Orderer, peer *nwo.Peer, channel string) {
	By("responding with a 300")
	sess, err := n.PeerUserSession(peer, "User1", commands.ChaincodeInvoke{
		ChannelID: channel,
		Orderer:   n.OrdererAddress(orderer, nwo.ListenPort),
		Name:      "mycc",
		Ctor:      `{"Args":["respond","300","response-message","response-payload"]}`,
		PeerAddresses: []string{
			n.PeerAddress(n.Peer("Org1", "peer0"), nwo.ListenPort),
			n.PeerAddress(n.Peer("Org2", "peer0"), nwo.ListenPort),
		},
		WaitForEvent: true,
	})
	Expect(err).NotTo(HaveOccurred())
	Eventually(sess, n.EventuallyTimeout).Should(gexec.Exit(0))
	Expect(sess.Err).To(gbytes.Say("Chaincode invoke successful. result: status:300"))

	By("responding with a 400")
	sess, err = n.PeerUserSession(peer, "User1", commands.ChaincodeInvoke{
		ChannelID: channel,
		Orderer:   n.OrdererAddress(orderer, nwo.ListenPort),
		Name:      "mycc",
		Ctor:      `{"Args":["respond","400","response-message","response-payload"]}`,
		PeerAddresses: []string{
			n.PeerAddress(n.Peer("Org1", "peer0"), nwo.ListenPort),
			n.PeerAddress(n.Peer("Org2", "peer0"), nwo.ListenPort),
		},
		WaitForEvent: true,
	})
	Expect(err).NotTo(HaveOccurred())
	Eventually(sess, n.EventuallyTimeout).Should(gexec.Exit(1))
	Expect(sess.Err).To(gbytes.Say(`Error: endorsement failure during invoke.`))
}

func CheckPeerStatsdMetrics(prefix string, mr *MetricsReader, timeout time.Duration) {
	By("checking for peer statsd metrics")
	Eventually(mr.String, timeout).Should(SatisfyAll(
		ContainSubstring(prefix+".logging.entries_checked.info:"),
		ContainSubstring(prefix+".logging.entries_written.info:"),
		ContainSubstring(prefix+".go.mem.gc_completed_count:"),
		ContainSubstring(prefix+".grpc.server.unary_requests_received.protos_Endorser.ProcessProposal:"),
		ContainSubstring(prefix+".grpc.server.unary_requests_completed.protos_Endorser.ProcessProposal.OK:"),
		ContainSubstring(prefix+".grpc.server.unary_request_duration.protos_Endorser.ProcessProposal.OK:"),
		ContainSubstring(prefix+".ledger.blockchain_height"),
		ContainSubstring(prefix+".ledger.blockstorage_commit_time"),
		ContainSubstring(prefix+".ledger.blockstorage_and_pvtdata_commit_time"),
	))
}

func CheckPeerStatsdStreamMetrics(mr *MetricsReader, timeout time.Duration) {
	By("checking for stream metrics")
	Eventually(mr.String, timeout).Should(SatisfyAll(
		ContainSubstring(".grpc.server.stream_requests_received.protos_Deliver.DeliverFiltered:"),
		ContainSubstring(".grpc.server.stream_requests_completed.protos_Deliver.DeliverFiltered.Unknown:"),
		ContainSubstring(".grpc.server.stream_request_duration.protos_Deliver.DeliverFiltered.Unknown:"),
		ContainSubstring(".grpc.server.stream_messages_received.protos_Deliver.DeliverFiltered"),
		ContainSubstring(".grpc.server.stream_messages_sent.protos_Deliver.DeliverFiltered"),
	))
}

func CheckOrdererStatsdMetrics(prefix string, mr *MetricsReader, timeout time.Duration) {
	Eventually(mr.String, timeout).Should(SatisfyAll(
		ContainSubstring(prefix+".grpc.server.stream_request_duration.orderer_AtomicBroadcast.Broadcast.OK"),
		ContainSubstring(prefix+".grpc.server.stream_request_duration.orderer_AtomicBroadcast.Deliver."),
		ContainSubstring(prefix+".logging.entries_checked.info:"),
		ContainSubstring(prefix+".logging.entries_written.info:"),
		ContainSubstring(prefix+".go.mem.gc_completed_count:"),
		ContainSubstring(prefix+".grpc.server.stream_requests_received.orderer_AtomicBroadcast.Deliver:"),
		ContainSubstring(prefix+".grpc.server.stream_requests_completed.orderer_AtomicBroadcast.Deliver."),
		ContainSubstring(prefix+".grpc.server.stream_messages_received.orderer_AtomicBroadcast.Deliver"),
		ContainSubstring(prefix+".grpc.server.stream_messages_sent.orderer_AtomicBroadcast.Deliver"),
		ContainSubstring(prefix+".ledger.blockchain_height"),
		ContainSubstring(prefix+".ledger.blockstorage_commit_time"),
	))
}

func OrdererOperationalClients(network *nwo.Network, orderer *nwo.Orderer) (authClient, unauthClient *http.Client) {
	return operationalClients(network.OrdererLocalTLSDir(orderer))
}

func PeerOperationalClients(network *nwo.Network, peer *nwo.Peer) (authClient, unauthClient *http.Client) {
	return operationalClients(network.PeerLocalTLSDir(peer))
}

func operationalClients(tlsDir string) (authClient, unauthClient *http.Client) {
	clientCert, err := tls.LoadX509KeyPair(
		filepath.Join(tlsDir, "server.crt"),
		filepath.Join(tlsDir, "server.key"),
	)
	Expect(err).NotTo(HaveOccurred())

	clientCertPool := x509.NewCertPool()
	caCert, err := ioutil.ReadFile(filepath.Join(tlsDir, "ca.crt"))
	Expect(err).NotTo(HaveOccurred())
	clientCertPool.AppendCertsFromPEM(caCert)

	authenticatedClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				Certificates: []tls.Certificate{clientCert},
				RootCAs:      clientCertPool,
			},
		},
	}
	unauthenticatedClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: clientCertPool},
		},
	}

	return authenticatedClient, unauthenticatedClient
}

func CheckPeerOperationEndpoints(network *nwo.Network, peer *nwo.Peer) {
	metricsURL := fmt.Sprintf("https://127.0.0.1:%d/metrics", network.PeerPort(peer, nwo.OperationsPort))
	logspecURL := fmt.Sprintf("https://127.0.0.1:%d/logspec", network.PeerPort(peer, nwo.OperationsPort))
	healthURL := fmt.Sprintf("https://127.0.0.1:%d/healthz", network.PeerPort(peer, nwo.OperationsPort))

	authClient, unauthClient := PeerOperationalClients(network, peer)

	CheckPeerPrometheusMetrics(authClient, metricsURL)
	CheckLogspecOperations(authClient, logspecURL)
	CheckHealthEndpoint(authClient, healthURL)

	By("getting the logspec without a client cert")
	resp, err := unauthClient.Get(logspecURL)
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized))

	By("ensuring health checks do not require a client cert")
	CheckHealthEndpoint(unauthClient, healthURL)
}

func CheckOrdererOperationEndpoints(network *nwo.Network, orderer *nwo.Orderer) {
	metricsURL := fmt.Sprintf("https://127.0.0.1:%d/metrics", network.OrdererPort(orderer, nwo.OperationsPort))
	logspecURL := fmt.Sprintf("https://127.0.0.1:%d/logspec", network.OrdererPort(orderer, nwo.OperationsPort))
	healthURL := fmt.Sprintf("https://127.0.0.1:%d/healthz", network.OrdererPort(orderer, nwo.OperationsPort))

	authClient, unauthClient := OrdererOperationalClients(network, orderer)

	CheckOrdererPrometheusMetrics(authClient, metricsURL)
	CheckLogspecOperations(authClient, logspecURL)
	CheckHealthEndpoint(authClient, healthURL)

	By("getting the logspec without a client cert")
	resp, err := unauthClient.Get(logspecURL)
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized))

	By("ensuring health checks do not require a client cert")
	CheckHealthEndpoint(unauthClient, healthURL)
}

func CheckPeerPrometheusMetrics(client *http.Client, url string) {
	By("hitting the prometheus metrics endpoint")
	resp, err := client.Get(url)
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.StatusCode).To(Equal(http.StatusOK))
	resp.Body.Close()

	Eventually(getBody(client, url)).Should(ContainSubstring(`# TYPE grpc_server_stream_request_duration histogram`))

	By("checking for some expected metrics")
	body := getBody(client, url)()
	Expect(body).To(ContainSubstring(`# TYPE go_gc_duration_seconds summary`))
	Expect(body).To(ContainSubstring(`# TYPE grpc_server_stream_request_duration histogram`))
	Expect(body).To(ContainSubstring(`grpc_server_stream_request_duration_count{code="Unknown",method="DeliverFiltered",service="protos_Deliver"}`))
	Expect(body).To(ContainSubstring(`grpc_server_stream_messages_received{method="DeliverFiltered",service="protos_Deliver"}`))
	Expect(body).To(ContainSubstring(`grpc_server_stream_messages_sent{method="DeliverFiltered",service="protos_Deliver"}`))
	Expect(body).To(ContainSubstring(`# TYPE grpc_comm_conn_closed counter`))
	Expect(body).To(ContainSubstring(`# TYPE grpc_comm_conn_opened counter`))
	Expect(body).To(ContainSubstring(`ledger_blockchain_height`))
	Expect(body).To(ContainSubstring(`ledger_blockstorage_commit_time_bucket`))
	Expect(body).To(ContainSubstring(`ledger_blockstorage_and_pvtdata_commit_time_bucket`))
}

func CheckOrdererPrometheusMetrics(client *http.Client, url string) {
	By("hitting the prometheus metrics endpoint")
	resp, err := client.Get(url)
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.StatusCode).To(Equal(http.StatusOK))
	resp.Body.Close()

	Eventually(getBody(client, url)).Should(ContainSubstring(`# TYPE grpc_server_stream_request_duration histogram`))

	By("checking for some expected metrics")
	body := getBody(client, url)()
	Expect(body).To(ContainSubstring(`# TYPE go_gc_duration_seconds summary`))
	Expect(body).To(ContainSubstring(`# TYPE grpc_server_stream_request_duration histogram`))
	Expect(body).To(ContainSubstring(`grpc_server_stream_request_duration_sum{code="OK",method="Deliver",service="orderer_AtomicBroadcast"`))
	Expect(body).To(ContainSubstring(`grpc_server_stream_request_duration_sum{code="OK",method="Broadcast",service="orderer_AtomicBroadcast"`))
	Expect(body).To(ContainSubstring(`# TYPE grpc_comm_conn_closed counter`))
	Expect(body).To(ContainSubstring(`# TYPE grpc_comm_conn_opened counter`))
	Expect(body).To(ContainSubstring(`ledger_blockchain_height`))
	Expect(body).To(ContainSubstring(`ledger_blockstorage_commit_time_bucket`))
}

func CheckLogspecOperations(client *http.Client, logspecURL string) {
	By("getting the logspec")
	resp, err := client.Get(logspecURL)
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.StatusCode).To(Equal(http.StatusOK))
	bodyBytes, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	Expect(err).NotTo(HaveOccurred())
	Expect(string(bodyBytes)).To(MatchJSON(`{"spec":"info"}`))

	updateReq, err := http.NewRequest(http.MethodPut, logspecURL, strings.NewReader(`{"spec":"debug"}`))
	Expect(err).NotTo(HaveOccurred())

	By("setting the logspec")
	resp, err = client.Do(updateReq)
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.StatusCode).To(Equal(http.StatusNoContent))
	resp.Body.Close()

	resp, err = client.Get(logspecURL)
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.StatusCode).To(Equal(http.StatusOK))
	bodyBytes, err = ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	Expect(err).NotTo(HaveOccurred())
	Expect(string(bodyBytes)).To(MatchJSON(`{"spec":"debug"}`))

	By("resetting the logspec")
	updateReq, err = http.NewRequest(http.MethodPut, logspecURL, strings.NewReader(`{"spec":"info"}`))
	Expect(err).NotTo(HaveOccurred())
	resp, err = client.Do(updateReq)
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.StatusCode).To(Equal(http.StatusNoContent))
	resp.Body.Close()
}

func CheckHealthEndpoint(client *http.Client, url string) {
	body := getBody(client, url)()

	var healthStatus healthz.HealthStatus
	err := json.Unmarshal([]byte(body), &healthStatus)
	Expect(err).NotTo(HaveOccurred())
	Expect(healthStatus.Status).To(Equal(healthz.StatusOK))
}

func getBody(client *http.Client, url string) func() string {
	return func() string {
		resp, err := client.Get(url)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		bodyBytes, err := ioutil.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())
		resp.Body.Close()
		return string(bodyBytes)
	}
}

func packageInstallApproveChaincode(network *nwo.Network, channel string, orderer *nwo.Orderer, chaincode nwo.Chaincode, peers ...*nwo.Peer) {
	nwo.PackageChaincode(network, chaincode, peers[0])
	nwo.InstallChaincode(network, chaincode, peers...)
	nwo.ApproveChaincodeForMyOrg(network, channel, orderer, chaincode, peers...)
}

func hashFile(file string) string {
	f, err := os.Open(file)
	Expect(err).NotTo(HaveOccurred())
	defer f.Close()

	h := sha256.New()
	_, err = io.Copy(h, f)
	Expect(err).NotTo(HaveOccurred())

	return fmt.Sprintf("%x", h.Sum(nil))
}

func chaincodeContainerNameFilter(n *nwo.Network, chaincode nwo.Chaincode) string {
	return fmt.Sprintf("^/%s-.*-%s-%s$", n.NetworkID, chaincode.Label, hashFile(chaincode.PackageFile))
}
