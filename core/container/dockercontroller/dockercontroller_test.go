/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package dockercontroller

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/fsouza/go-dockerclient"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric/common/ledger/testutil"
	"github.com/hyperledger/fabric/common/util"
	"github.com/hyperledger/fabric/core/chaincode/platforms"
	"github.com/hyperledger/fabric/core/container/ccintf"
	coreutil "github.com/hyperledger/fabric/core/testutil"
	pb "github.com/hyperledger/fabric/protos/peer"
)

// This test used to be part of an integration style test in core/container, moved to here
func TestRealPath(t *testing.T) {
	coreutil.SetupTestConfig()
	ctxt := context.Background()
	dc := NewDockerVM("", "")
	ccid := ccintf.CCID{Name: "simple"}
	reader := getCodeChainBytesInMem()

	err := dc.Deploy(ctxt, ccid, nil, nil, reader)
	require.NoError(t, err)

	err = dc.Start(ctxt, ccid, nil, nil, nil, nil)
	require.NoError(t, err)

	// Stop, killing, and deleting
	err = dc.Stop(ctxt, ccid, 0, true, true)
	require.NoError(t, err)

	err = dc.Start(ctxt, ccid, nil, nil, nil, nil)
	require.NoError(t, err)

	// Stop, killing, but not deleting
	_ = dc.Stop(ctxt, ccid, 0, false, true)
}

func TestHostConfig(t *testing.T) {
	coreutil.SetupTestConfig()
	var hostConfig = new(docker.HostConfig)
	err := viper.UnmarshalKey("vm.docker.hostConfig", hostConfig)
	if err != nil {
		t.Fatalf("Load docker HostConfig wrong, error: %s", err.Error())
	}
	testutil.AssertNotEquals(t, hostConfig.LogConfig, nil)
	testutil.AssertEquals(t, hostConfig.LogConfig.Type, "json-file")
	testutil.AssertEquals(t, hostConfig.LogConfig.Config["max-size"], "50m")
	testutil.AssertEquals(t, hostConfig.LogConfig.Config["max-file"], "5")
}

func TestGetDockerHostConfig(t *testing.T) {
	os.Setenv("CORE_VM_DOCKER_HOSTCONFIG_NETWORKMODE", "overlay")
	os.Setenv("CORE_VM_DOCKER_HOSTCONFIG_CPUSHARES", fmt.Sprint(1024*1024*1024*2))
	coreutil.SetupTestConfig()
	hostConfig = nil // There is a cached global singleton for docker host config, the other tests can collide with
	hostConfig := getDockerHostConfig()
	testutil.AssertNotNil(t, hostConfig)
	testutil.AssertEquals(t, hostConfig.NetworkMode, "overlay")
	testutil.AssertEquals(t, hostConfig.LogConfig.Type, "json-file")
	testutil.AssertEquals(t, hostConfig.LogConfig.Config["max-size"], "50m")
	testutil.AssertEquals(t, hostConfig.LogConfig.Config["max-file"], "5")
	testutil.AssertEquals(t, hostConfig.Memory, int64(1024*1024*1024*2))
	testutil.AssertEquals(t, hostConfig.CPUShares, int64(1024*1024*1024*2))
}

func Test_Deploy(t *testing.T) {
	dvm := DockerVM{}
	ccid := ccintf.CCID{Name: "simple"}
	//get the tarball for codechain
	tarRdr := getCodeChainBytesInMem()
	args := make([]string, 1)
	env := make([]string, 1)
	ctx := context.Background()

	// getMockClient returns error
	getClientErr = true
	dvm.getClientFnc = getMockClient
	err := dvm.Deploy(ctx, ccid, args, env, tarRdr)
	testerr(t, err, false)
	getClientErr = false

	// Failure case: dockerClient.BuildImage returns error
	buildErr = true
	dvm.getClientFnc = getMockClient
	err = dvm.Deploy(ctx, ccid, args, env, tarRdr)
	testerr(t, err, false)
	buildErr = false

	// Success case
	err = dvm.Deploy(ctx, ccid, args, env, tarRdr)
	testerr(t, err, true)
}

func Test_Start(t *testing.T) {
	dvm := DockerVM{}
	ccid := ccintf.CCID{Name: "simple"}
	args := make([]string, 1)
	env := make([]string, 1)
	files := map[string][]byte{
		"hello": []byte("world"),
	}
	ctx := context.Background()

	// Failure cases
	// case 1: getMockClient returns error
	dvm.getClientFnc = getMockClient
	getClientErr = true
	err := dvm.Start(ctx, ccid, args, env, files, nil)
	testerr(t, err, false)
	getClientErr = false

	// case 2: dockerClient.CreateContainer returns error
	createErr = true
	err = dvm.Start(ctx, ccid, args, env, files, nil)
	testerr(t, err, false)
	createErr = false

	// case 3: dockerClient.UploadToContainer returns error
	uploadErr = true
	err = dvm.Start(ctx, ccid, args, env, files, nil)
	testerr(t, err, false)
	uploadErr = false

	// case 4: dockerClient.StartContainer returns docker.noSuchImgErr
	noSuchImgErr = true
	err = dvm.Start(ctx, ccid, args, env, files, nil)
	testerr(t, err, false)

	chaincodePath := "github.com/hyperledger/fabric/examples/chaincode/go/example01/cmd"
	spec := &pb.ChaincodeSpec{Type: pb.ChaincodeSpec_GOLANG,
		ChaincodeId: &pb.ChaincodeID{Name: "ex01", Path: chaincodePath},
		Input:       &pb.ChaincodeInput{Args: util.ToChaincodeArgs("f")}}
	codePackage, err := platforms.GetDeploymentPayload(spec)
	if err != nil {
		t.Fatal()
	}
	cds := &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec, CodePackage: codePackage}
	bldr := &mockBuilder{
		buildFunc: func() (io.Reader, error) { return platforms.GenerateDockerBuild(cds) },
	}

	// case 4: start called with builder and dockerClient.CreateContainer returns
	// docker.noSuchImgErr and dockerClient.Start returns error
	viper.Set("vm.docker.attachStdout", true)
	startErr = true
	err = dvm.Start(ctx, ccid, args, env, files, bldr)
	testerr(t, err, false)
	startErr = false

	// Success cases
	err = dvm.Start(ctx, ccid, args, env, files, bldr)
	testerr(t, err, true)
	noSuchImgErr = false

	// dockerClient.StopContainer returns error
	stopErr = true
	err = dvm.Start(ctx, ccid, args, env, files, nil)
	testerr(t, err, true)
	stopErr = false

	// dockerClient.KillContainer returns error
	killErr = true
	err = dvm.Start(ctx, ccid, args, env, files, nil)
	testerr(t, err, true)
	killErr = false

	// dockerClient.RemoveContainer returns error
	removeErr = true
	err = dvm.Start(ctx, ccid, args, env, files, nil)
	testerr(t, err, true)
	removeErr = false

	err = dvm.Start(ctx, ccid, args, env, files, nil)
	testerr(t, err, true)
}

func Test_Stop(t *testing.T) {
	dvm := DockerVM{}
	ccid := ccintf.CCID{Name: "simple"}
	ctx := context.Background()

	// Failure case: getMockClient returns error
	getClientErr = true
	dvm.getClientFnc = getMockClient
	err := dvm.Stop(ctx, ccid, 10, true, true)
	testerr(t, err, false)
	getClientErr = false

	// Success case
	err = dvm.Stop(ctx, ccid, 10, true, true)
	testerr(t, err, true)
}

func Test_Destroy(t *testing.T) {
	dvm := DockerVM{}
	ccid := ccintf.CCID{Name: "simple"}
	ctx := context.Background()

	// Failure cases
	// Case 1: getMockClient returns error
	getClientErr = true
	dvm.getClientFnc = getMockClient
	err := dvm.Destroy(ctx, ccid, true, true)
	testerr(t, err, false)
	getClientErr = false

	// Case 2: dockerClient.RemoveImageExtended returns error
	removeImgErr = true
	err = dvm.Destroy(ctx, ccid, true, true)
	testerr(t, err, false)
	removeImgErr = false

	// Success case
	err = dvm.Destroy(ctx, ccid, true, true)
	testerr(t, err, true)
}

type testCase struct {
	name           string
	vm             *DockerVM
	ccid           ccintf.CCID
	expectedOutput string
}

func TestGetVMNameForDocker(t *testing.T) {
	tc := []testCase{
		{
			name:           "mycc",
			vm:             &DockerVM{NetworkID: "dev", PeerID: "peer0"},
			ccid:           ccintf.CCID{Name: "mycc", Version: "1.0"},
			expectedOutput: fmt.Sprintf("%s-%s", "dev-peer0-mycc-1.0", hex.EncodeToString(util.ComputeSHA256([]byte("dev-peer0-mycc-1.0")))),
		},
		{
			name:           "mycc-nonetworkid",
			vm:             &DockerVM{PeerID: "peer1"},
			ccid:           ccintf.CCID{Name: "mycc", Version: "1.0"},
			expectedOutput: fmt.Sprintf("%s-%s", "peer1-mycc-1.0", hex.EncodeToString(util.ComputeSHA256([]byte("peer1-mycc-1.0")))),
		},
		{
			name:           "myCC-UCids",
			vm:             &DockerVM{NetworkID: "Dev", PeerID: "Peer0"},
			ccid:           ccintf.CCID{Name: "myCC", Version: "1.0"},
			expectedOutput: fmt.Sprintf("%s-%s", "dev-peer0-mycc-1.0", hex.EncodeToString(util.ComputeSHA256([]byte("Dev-Peer0-myCC-1.0")))),
		},
		{
			name:           "myCC-idsWithSpecialChars",
			vm:             &DockerVM{NetworkID: "Dev$dev", PeerID: "Peer*0"},
			ccid:           ccintf.CCID{Name: "myCC", Version: "1.0"},
			expectedOutput: fmt.Sprintf("%s-%s", "dev-dev-peer-0-mycc-1.0", hex.EncodeToString(util.ComputeSHA256([]byte("Dev$dev-Peer*0-myCC-1.0")))),
		},
		{
			name:           "mycc-nopeerid",
			vm:             &DockerVM{NetworkID: "dev"},
			ccid:           ccintf.CCID{Name: "mycc", Version: "1.0"},
			expectedOutput: fmt.Sprintf("%s-%s", "dev-mycc-1.0", hex.EncodeToString(util.ComputeSHA256([]byte("dev-mycc-1.0")))),
		},
		{
			name:           "myCC-LCids",
			vm:             &DockerVM{NetworkID: "dev", PeerID: "peer0"},
			ccid:           ccintf.CCID{Name: "myCC", Version: "1.0"},
			expectedOutput: fmt.Sprintf("%s-%s", "dev-peer0-mycc-1.0", hex.EncodeToString(util.ComputeSHA256([]byte("dev-peer0-myCC-1.0")))),
		},
	}

	for _, test := range tc {
		name, err := test.vm.GetVMNameForDocker(test.ccid)
		assert.Nil(t, err, "Expected nil error")
		assert.Equal(t, test.expectedOutput, name, "Unexpected output for test case name: %s", test.name)
	}

}

func TestGetVMName(t *testing.T) {
	tc := []testCase{
		{
			name:           "myCC-preserveCase",
			vm:             &DockerVM{NetworkID: "Dev", PeerID: "Peer0"},
			ccid:           ccintf.CCID{Name: "myCC", Version: "1.0"},
			expectedOutput: fmt.Sprintf("%s", "Dev-Peer0-myCC-1.0"),
		},
	}

	for _, test := range tc {
		name := test.vm.GetVMName(test.ccid)
		assert.Equal(t, test.expectedOutput, name, "Unexpected output for test case name: %s", test.name)
	}

}

/*func TestFormatImageName_invalidChars(t *testing.T) {
	_, err := formatImageName("invalid*chars")
	assert.NotNil(t, err, "Expected error")
}*/

func getCodeChainBytesInMem() io.Reader {
	startTime := time.Now()
	inputbuf := bytes.NewBuffer(nil)
	gw := gzip.NewWriter(inputbuf)
	tr := tar.NewWriter(gw)
	dockerFileContents := []byte("FROM busybox:latest\n\nCMD echo hello")
	dockerFileSize := int64(len([]byte(dockerFileContents)))

	tr.WriteHeader(&tar.Header{Name: "Dockerfile", Size: dockerFileSize,
		ModTime: startTime, AccessTime: startTime, ChangeTime: startTime})
	tr.Write([]byte(dockerFileContents))
	tr.Close()
	gw.Close()
	return inputbuf
}

func testerr(t *testing.T, err error, succ bool) {
	if succ {
		assert.NoError(t, err, "Expected success but got error")
	} else {
		assert.Error(t, err, "Expected failure but succeeded")
	}
}

func getMockClient() (dockerClient, error) {
	if getClientErr {
		return nil, errors.New("Failed to get client")
	}
	return &mockClient{noSuchImgErrReturned: false}, nil
}

type mockBuilder struct {
	buildFunc func() (io.Reader, error)
}

func (m *mockBuilder) Build() (io.Reader, error) {
	return m.buildFunc()
}

type mockClient struct {
	noSuchImgErrReturned bool
}

var getClientErr, createErr, uploadErr, noSuchImgErr, buildErr, removeImgErr,
	startErr, stopErr, killErr, removeErr bool

func (c *mockClient) CreateContainer(options docker.CreateContainerOptions) (*docker.Container, error) {
	if createErr {
		return nil, errors.New("Error creating the container")
	} else if noSuchImgErr && !c.noSuchImgErrReturned {
		c.noSuchImgErrReturned = true
		return nil, docker.ErrNoSuchImage
	}
	return &docker.Container{}, nil
}

func (c *mockClient) StartContainer(id string, cfg *docker.HostConfig) error {
	if startErr {
		return errors.New("Error starting the container")
	}
	return nil
}

func (c *mockClient) UploadToContainer(id string, opts docker.UploadToContainerOptions) error {
	if uploadErr {
		return errors.New("Error uploading archive to the container")
	}
	return nil
}

func (c *mockClient) AttachToContainer(opts docker.AttachToContainerOptions) error {
	if opts.Success != nil {
		opts.Success <- struct{}{}
	}
	return nil
}

func (c *mockClient) BuildImage(opts docker.BuildImageOptions) error {
	if buildErr {
		return errors.New("Error building image")
	}
	return nil
}

func (c *mockClient) RemoveImageExtended(id string, opts docker.RemoveImageOptions) error {
	if removeImgErr {
		return errors.New("Error removing extended image")
	}
	return nil
}

func (c *mockClient) StopContainer(id string, timeout uint) error {
	if stopErr {
		return errors.New("Error stopping container")
	}
	return nil
}

func (c *mockClient) KillContainer(opts docker.KillContainerOptions) error {
	if killErr {
		return errors.New("Error killing container")
	}
	return nil
}

func (c *mockClient) RemoveContainer(opts docker.RemoveContainerOptions) error {
	if removeErr {
		return errors.New("Error removing container")
	}
	return nil
}
