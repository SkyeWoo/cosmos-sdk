package server

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/cosmos/cosmos-sdk/crypto/keys"
	"github.com/tendermint/tendermint/crypto"

	cfg "github.com/tendermint/tendermint/config"
	tmcli "github.com/tendermint/tendermint/libs/cli"
	cmn "github.com/tendermint/tendermint/libs/common"
	dbm "github.com/tendermint/tendermint/libs/db"
	"github.com/tendermint/tendermint/p2p"
	pvm "github.com/tendermint/tendermint/privval"
	tmtypes "github.com/tendermint/tendermint/types"

	clkeys "github.com/cosmos/cosmos-sdk/client/keys"
	"github.com/cosmos/cosmos-sdk/codec"
	sdk "github.com/cosmos/cosmos-sdk/types"
	auth "github.com/cosmos/cosmos-sdk/x/auth"
	"github.com/cosmos/cosmos-sdk/x/stake"
)

//Parameter names, for init gen-tx command
var (
	FlagName       = "name"
	FlagClientHome = "home-client"
	FlagOWK        = "owk"
)

//parameter names, init command
var (
	FlagOverwrite = "overwrite"
	FlagWithTxs   = "with-txs"
	FlagIP        = "ip"
	FlagChainID   = "chain-id"
)

// genesis piece structure for creating combined genesis
type GenesisTx struct {
	NodeID    string                   `json:"node_id"`
	IP        string                   `json:"ip"`
	Validator tmtypes.GenesisValidator `json:"validator"`
	AppGenTx  json.RawMessage          `json:"app_gen_tx"`
}

// Storage for init command input parameters
type InitConfig struct {
	ChainID   string
	GenTxs    bool
	GenTxsDir string
	Overwrite bool
}

// get cmd to initialize all files for tendermint and application
func InitCmd(ctx *Context, cdc *codec.Codec, appInit AppInit) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize genesis config, priv-validator file, and p2p-node file",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {

			config := ctx.Config
			config.SetRoot(viper.GetString(tmcli.HomeFlag))
			initConfig := InitConfig{
				ChainID:   viper.GetString(FlagChainID),
				GenTxs:    viper.GetBool(FlagWithTxs),
				GenTxsDir: filepath.Join(config.RootDir, "config", "gentx"),
				Overwrite: viper.GetBool(FlagOverwrite),
			}

			chainID, nodeID, appMessage, err := initWithConfig(cdc, appInit, config, initConfig)
			if err != nil {
				return err
			}
			// print out some key information
			toPrint := struct {
				ChainID    string          `json:"chain_id"`
				NodeID     string          `json:"node_id"`
				AppMessage json.RawMessage `json:"app_message"`
			}{
				chainID,
				nodeID,
				appMessage,
			}
			out, err := codec.MarshalJSONIndent(cdc, toPrint)
			if err != nil {
				return err
			}
			fmt.Println(string(out))
			return nil
		},
	}
	cmd.Flags().BoolP(FlagOverwrite, "o", false, "overwrite the genesis.json file")
	cmd.Flags().String(FlagChainID, "", "genesis file chain-id, if left blank will be randomly created")
	cmd.Flags().Bool(FlagWithTxs, false, "apply existing genesis transactions from [--home]/config/gentx/")
	cmd.Flags().AddFlagSet(appInit.FlagsAppGenState)
	cmd.Flags().AddFlagSet(appInit.FlagsAppGenTx) // need to add this flagset for when no GenTx's provided
	return cmd
}

func initWithConfig(cdc *codec.Codec, appInit AppInit, config *cfg.Config, initConfig InitConfig) (
	chainID string, nodeID string, appMessage json.RawMessage, err error) {
	nodeKey, err := p2p.LoadOrGenNodeKey(config.NodeKeyFile())
	if err != nil {
		return
	}
	nodeID = string(nodeKey.ID())
	//pubKey := readOrCreatePrivValidator(config)

	if initConfig.ChainID == "" {
		initConfig.ChainID = fmt.Sprintf("test-chain-%v", cmn.RandStr(6))
	}
	chainID = initConfig.ChainID

	genFile := config.GenesisFile()
	if !initConfig.Overwrite && cmn.FileExists(genFile) {
		err = fmt.Errorf("genesis.json file already exists: %v", genFile)
		return
	}

	// process genesis transactions, or otherwise create one for defaults
	var appGenTxs []auth.StdTx
	var validators []tmtypes.GenesisValidator
	var persistentPeers string

	if initConfig.GenTxs {
		appGenTxs, persistentPeers, err = processStdTxs(initConfig.GenTxsDir, cdc)
		if err != nil {
			return
		}
		config.P2P.PersistentPeers = persistentPeers
		configFilePath := filepath.Join(config.RootDir, "config", "config.toml")
		cfg.WriteConfigFile(configFilePath, config)
	}
	// } else {
	// 	genTxConfig := serverconfig.GenTx{
	// 		viper.GetString(FlagName),
	// 		viper.GetString(FlagClientHome),
	// 		viper.GetBool(FlagOWK),
	// 		"127.0.0.1",
	// 	}

	// 	// Write updated config with moniker
	// 	config.Moniker = genTxConfig.Name
	// 	configFilePath := filepath.Join(config.RootDir, "config", "config.toml")
	// 	cfg.WriteConfigFile(configFilePath, config)
	// 	appGenTx, am, validator, err := appInit.AppGenTx(cdc, pubKey, genTxConfig)
	// 	appMessage = am
	// 	if err != nil {
	// 		return "", "", nil, err
	// 	}
	// 	validators = []tmtypes.GenesisValidator{validator}
	// 	appGenTxs = []json.RawMessage{appGenTx}
	// }

	appState, err := appInit.AppGenState(cdc, appGenTxs)
	if err != nil {
		return
	}

	err = writeGenesisFile(cdc, genFile, initConfig.ChainID, validators, appState)
	if err != nil {
		return
	}

	return
}

func processStdTxs(genTxsDir string, cdc *codec.Codec) (txs []auth.StdTx, persistentPeers string, err error) {
	var fos []os.FileInfo
	fos, err = ioutil.ReadDir(genTxsDir)
	if err != nil {
		return
	}

	var addresses []string
	for _, fo := range fos {
		filename := path.Join(genTxsDir, fo.Name())
		if !fo.IsDir() && (path.Ext(filename) != ".json") {
			continue
		}

		// get the genTx
		var bz []byte
		bz, err = ioutil.ReadFile(filename)
		if err != nil {
			return
		}
		var genTx auth.StdTx
		err = cdc.UnmarshalJSON(bz, &genTx)
		if err != nil {
			return
		}
		txs = append(txs, genTx)

		nodeAddr := genTx.GetMemo()
		if len(nodeAddr) == 0 {
			err = fmt.Errorf("couldn't find node's address in %s", fo.Name())
			return
		}
		addresses = append(addresses, nodeAddr)
	}

	sort.Strings(addresses)
	persistentPeers = strings.Join(addresses, ",")

	return
}

// append a genesis-piece
func processGenTxs(genTxsDir string, cdc *codec.Codec) (
	validators []tmtypes.GenesisValidator, appGenTxs []json.RawMessage, persistentPeers string, err error) {

	var fos []os.FileInfo
	fos, err = ioutil.ReadDir(genTxsDir)
	if err != nil {
		return
	}

	genTxs := make(map[string]GenesisTx)
	var nodeIDs []string
	for _, fo := range fos {
		filename := path.Join(genTxsDir, fo.Name())
		if !fo.IsDir() && (path.Ext(filename) != ".json") {
			continue
		}

		// get the genTx
		var bz []byte
		bz, err = ioutil.ReadFile(filename)
		if err != nil {
			return
		}
		var genTx GenesisTx
		err = cdc.UnmarshalJSON(bz, &genTx)
		if err != nil {
			return
		}

		genTxs[genTx.NodeID] = genTx
		nodeIDs = append(nodeIDs, genTx.NodeID)
	}

	sort.Strings(nodeIDs)

	for _, nodeID := range nodeIDs {
		genTx := genTxs[nodeID]

		// combine some stuff
		validators = append(validators, genTx.Validator)
		appGenTxs = append(appGenTxs, genTx.AppGenTx)

		// Add a persistent peer
		comma := ","
		if len(persistentPeers) == 0 {
			comma = ""
		}
		persistentPeers += fmt.Sprintf("%s%s@%s:26656", comma, genTx.NodeID, genTx.IP)
	}

	return
}

//________________________________________________________________________________________

// read of create the private key file for this config
func readOrCreatePrivValidator(tmConfig *cfg.Config) crypto.PubKey {
	// private validator
	privValFile := tmConfig.PrivValidatorFile()
	var privValidator *pvm.FilePV
	if cmn.FileExists(privValFile) {
		privValidator = pvm.LoadFilePV(privValFile)
	} else {
		privValidator = pvm.GenFilePV(privValFile)
		privValidator.Save()
	}
	return privValidator.GetPubKey()
}

// writeGenesisFile creates and writes the genesis configuration to disk. An
// error is returned if building or writing the configuration to file fails.
// nolint: unparam
func writeGenesisFile(cdc *codec.Codec, genesisFile, chainID string, validators []tmtypes.GenesisValidator, appState json.RawMessage) error {
	genDoc := tmtypes.GenesisDoc{
		ChainID:    chainID,
		Validators: validators,
		AppState:   appState,
	}

	if err := genDoc.ValidateAndComplete(); err != nil {
		return err
	}

	return genDoc.SaveAs(genesisFile)
}

//_____________________________________________________________________

// Core functionality passed from the application to the server init command
type AppInit struct {

	// flags required for application init functions
	FlagsAppGenState *pflag.FlagSet
	FlagsAppGenTx    *pflag.FlagSet

	// AppGenState creates the core parameters initialization. It takes in a
	// pubkey meant to represent the pubkey of the validator of this machine.
	AppGenState func(cdc *codec.Codec, appGenTx []auth.StdTx) (appState json.RawMessage, err error)
}

//_____________________________________________________________________

// simple default application init
var DefaultAppInit = AppInit{
	//	AppGenTx:    SimpleAppGenTx,
	AppGenState: SimpleAppGenState,
}

// create the genesis app state
func SimpleAppGenState(cdc *codec.Codec, appGenTxs []auth.StdTx) (appState json.RawMessage, err error) {

	if len(appGenTxs) != 1 {
		err = errors.New("must provide a single genesis transaction")
		return
	}

	msgs := appGenTxs[0].GetMsgs()
	if len(msgs) != 1 {
		err = errors.New("must provide a single genesis message")
		return
	}

	msg := msgs[0].(stake.MsgCreateValidator)
	appState = json.RawMessage(fmt.Sprintf(`{
  "accounts": [{
    "address": "%s",
    "coins": [
      {
        "denom": "mycoin",
        "amount": "9007199254740992"
      }
    ]
  }]
}`, msg.ValidatorAddr))
	return
}

//___________________________________________________________________________________________

// GenerateCoinKey returns the address of a public key, along with the secret
// phrase to recover the private key.
func GenerateCoinKey() (sdk.AccAddress, string, error) {

	// construct an in-memory key store
	keybase := keys.New(
		dbm.NewMemDB(),
	)

	// generate a private key, with recovery phrase
	info, secret, err := keybase.CreateMnemonic("name", keys.English, "pass", keys.Secp256k1)
	if err != nil {
		return sdk.AccAddress([]byte{}), "", err
	}
	addr := info.GetPubKey().Address()
	return sdk.AccAddress(addr), secret, nil
}

// GenerateSaveCoinKey returns the address of a public key, along with the secret
// phrase to recover the private key.
func GenerateSaveCoinKey(clientRoot, keyName, keyPass string, overwrite bool) (sdk.AccAddress, string, error) {

	// get the keystore from the client
	keybase, err := clkeys.GetKeyBaseFromDir(clientRoot)
	if err != nil {
		return sdk.AccAddress([]byte{}), "", err
	}

	// ensure no overwrite
	if !overwrite {
		_, err := keybase.Get(keyName)
		if err == nil {
			return sdk.AccAddress([]byte{}), "", errors.New("key already exists, overwrite is disabled")
		}
	}

	// generate a private key, with recovery phrase
	info, secret, err := keybase.CreateMnemonic(keyName, keys.English, keyPass, keys.Secp256k1)
	if err != nil {
		return sdk.AccAddress([]byte{}), "", err
	}
	addr := info.GetPubKey().Address()
	return sdk.AccAddress(addr), secret, nil
}
