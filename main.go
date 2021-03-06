package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math"
	"sort"
	"strings"
	"time"

	amino "github.com/tendermint/go-amino"
	tmtypes "github.com/tendermint/tendermint/types"

	gaia "github.com/cosmos/cosmos-sdk/cmd/gaia/app"
	sdk "github.com/cosmos/cosmos-sdk/types"

	"github.com/cosmos/launch/pkg"
)

const (
	// processed contributors files
	icfJSON     = "accounts/icf/contributors.json"
	privateJSON = "accounts/private/contributors.json"
	publicJSON  = "accounts/public/contributors.json"

	// seperate because vesting
	aibEmployeeJSON = "accounts/aib/employees.json"
	aibMultisigJSON = "accounts/aib/multisig.json"

	genesisTemplate = "params/genesis_template.json"
	genesisFile     = "penultimate_genesis.json"

	atomDenomination    = "uatom"
	atomGenesisTotal    = 236198958.12
	addressGenesisTotal = 984

	timeGenesisString = "2019-03-13 23:00:00 -0000 UTC"
)

// constants but can't use `const`
var (
	timeGenesis time.Time

	// vesting times
	timeGenesisTwoMonths time.Time
	timeGenesisOneYear   time.Time
	timeGenesisTwoYears  time.Time
)

// initialize the times!
func init() {
	var err error
	timeLayoutString := "2006-01-02 15:04:05 -0700 MST"
	timeGenesis, err = time.Parse(timeLayoutString, timeGenesisString)
	if err != nil {
		panic(err)
	}
	timeGenesisTwoMonths = timeGenesis.AddDate(0, 2, 0)
	timeGenesisOneYear = timeGenesis.AddDate(1, 0, 0)
	timeGenesisTwoYears = timeGenesis.AddDate(2, 0, 0)
}

// max precision on amt is two decimals ("centi-atoms")
func atomToUAtomInt(amt float64) sdk.Int {
	// amt is specified to 2 decimals ("centi-atoms").
	// multiply by 100 to get the number of centi-atoms
	// and round to int64.
	// Multiply by remaining to get uAtoms.
	var precision float64 = 100
	var remaining int64 = 10000

	catoms := int64(math.Round(amt * precision))
	uAtoms := catoms * remaining
	return sdk.NewInt(uAtoms)
}

// convert atoms with two decimal precision to coins
func newCoins(amt float64) sdk.Coins {
	uAtoms := atomToUAtomInt(amt)
	return sdk.Coins{
		sdk.Coin{
			Denom:  atomDenomination,
			Amount: uAtoms,
		},
	}
}

func main() {
	// for each path, accumulate the contributors file
	// icf addresses are in bech32, fundraiser are in hex
	contribs := make(map[string]float64)
	{
		accumulateBechContributors(icfJSON, contribs)
		accumulateHexContributors(privateJSON, contribs)
		accumulateHexContributors(publicJSON, contribs)
	}

	// load the aib pieces
	employees, multisig := aibAtoms(aibEmployeeJSON, aibMultisigJSON, contribs)

	// construct the genesis accounts :)
	var genesisAccounts []gaia.GenesisAccount
	{
		for addr, amt := range contribs {
			acc := gaia.GenesisAccount{
				Address: fromBech32(addr),
				Coins:   newCoins(amt),
			}
			genesisAccounts = append(genesisAccounts, acc)
		}

		// add aib employees vesting for 1 year cliff
		for _, aibAcc := range employees {
			coins := newCoins(aibAcc.Amount)
			genAcc := gaia.GenesisAccount{
				Address:         fromBech32(aibAcc.Address),
				Coins:           coins,
				OriginalVesting: coins,
				EndTime:         timeGenesisOneYear.Unix(),
			}
			genesisAccounts = append(genesisAccounts, genAcc)
		}

		// add aib multisig vesting continuosuly for 2 years
		// starting after 2 months
		multisigCoins := newCoins(multisig.Amount)
		genAcc := gaia.GenesisAccount{
			Address:         fromBech32(multisig.Address),
			Coins:           multisigCoins,
			OriginalVesting: multisigCoins,
			StartTime:       timeGenesisTwoMonths.Unix(),
			EndTime:         timeGenesisTwoYears.Unix(),
		}
		genesisAccounts = append(genesisAccounts, genAcc)
	}

	// check uAtom total
	uAtomTotal := sdk.NewInt(0)
	for _, account := range genesisAccounts {
		uAtomTotal = uAtomTotal.Add(account.Coins[0].Amount)
	}
	if !uAtomTotal.Equal(atomToUAtomInt(atomGenesisTotal)) {
		panicStr := fmt.Sprintf("expected %s atoms, got %s atoms allocated in genesis", atomToUAtomInt(atomGenesisTotal), uAtomTotal.String())
		panic(panicStr)
	}
	if len(genesisAccounts) != addressGenesisTotal {
		panicStr := fmt.Sprintf("expected %d addresses, got %d addresses allocated in genesis", addressGenesisTotal, len(genesisAccounts))
		panic(panicStr)
	}

	fmt.Println("-----------")
	fmt.Println("TOTAL addrs", len(genesisAccounts))
	fmt.Println("TOTAL uAtoms", uAtomTotal.String())

	// ensure no duplicates
	{
		checkdupls := make(map[string]struct{})
		for _, acc := range genesisAccounts {
			if _, ok := checkdupls[acc.Address.String()]; ok {
				panic(fmt.Sprintf("Got duplicate: %v", acc.Address))
			}
			checkdupls[acc.Address.String()] = struct{}{}
		}
		if len(checkdupls) != len(genesisAccounts) {
			panic("length mismatch!")
		}
	}

	// sort the accounts
	sort.SliceStable(genesisAccounts, func(i, j int) bool {
		return strings.Compare(
			genesisAccounts[i].Address.String(),
			genesisAccounts[j].Address.String(),
		) < 0
	})

	var genesisDoc *tmtypes.GenesisDoc
	// XXX: this is a bit much. is there something we can more easily resuse here?
	// and do we need to register amino here?
	// Note the app state is decoded using amino (ints are strings, anything else ?)
	cdc := amino.NewCodec()
	{
		// read the template with the params
		var err error
		genesisDoc, err = tmtypes.GenesisDocFromFile(genesisTemplate)
		if err != nil {
			panic(err)
		}
		// set genesis time
		genesisDoc.GenesisTime = timeGenesis

		// read the gaia state from the generic tendermint app state bytes
		// and populate with the accounts.
		var genesisState gaia.GenesisState
		err = cdc.UnmarshalJSON(genesisDoc.AppState, &genesisState)
		if err != nil {
			panic(err)
		}
		genesisState.Accounts = genesisAccounts

		// marshal the gaia app state back to json and update the genesisDoc
		genesisStateJSON, err := cdc.MarshalJSON(genesisState)
		if err != nil {
			panic(err)
		}
		genesisDoc.AppState = genesisStateJSON
	}

	// write the genesis file
	{

		bz, err := cdc.MarshalJSON(genesisDoc)
		if err != nil {
			panic(err)
		}
		buf := bytes.NewBuffer([]byte{})
		err = json.Indent(buf, bz, "", "  ")
		if err != nil {
			panic(err)
		}
		err = ioutil.WriteFile(genesisFile, buf.Bytes(), 0600)
		if err != nil {
			panic(err)
		}
	}
}

func fromBech32(address string) sdk.AccAddress {
	bech32PrefixAccAddr := "cosmos"
	bz, err := sdk.GetFromBech32(address, bech32PrefixAccAddr)
	if err != nil {
		panic(err)
	}
	if len(bz) != sdk.AddrLen {
		panic("Incorrect address length")
	}
	return sdk.AccAddress(bz)
}

// load a map of hex addresses and convert them to bech32
func accumulateHexContributors(fileName string, contribs map[string]float64) error {
	allocations := pkg.ObjToMap(fileName)

	for addr, amt := range allocations {
		bech32Addr, err := sdk.AccAddressFromHex(addr)
		if err != nil {
			return err
		}
		addr = bech32Addr.String()

		if _, ok := contribs[addr]; ok {
			fmt.Println("Duplicate addr", addr)
		}
		contribs[addr] += amt
	}
	return nil
}

func accumulateBechContributors(fileName string, contribs map[string]float64) error {
	allocations := pkg.ObjToMap(fileName)

	for addr, amt := range allocations {
		if _, ok := contribs[addr]; ok {
			fmt.Println("Duplicate addr", addr)
		}
		contribs[addr] += amt
	}
	return nil
}

//----------------------------------------------------------
// AiB Data

type Account struct {
	Address string  `json:"addr"`
	Amount  float64 `json:"amount"`
	Lock    string  `json:"lock"`
}

type MultisigAccount struct {
	Address   string   `json:"addr"`
	Threshold int      `json:"threshold"`
	Pubs      []string `json:"pubs"`
	Amount    float64  `json:"amount"`
}

// load the aib atoms and ensure there are no duplicates with the contribs
func aibAtoms(employeesFile, multisigFile string, contribs map[string]float64) (employees []Account, multisigAcc MultisigAccount) {
	bz, err := ioutil.ReadFile(employeesFile)
	if err != nil {
		panic(err)
	}
	err = json.Unmarshal(bz, &employees)
	if err != nil {
		panic(err)
	}

	bz, err = ioutil.ReadFile(multisigFile)
	if err != nil {
		panic(err)
	}
	err = json.Unmarshal(bz, &multisigAcc)
	if err != nil {
		panic(err)
	}

	for _, acc := range employees {
		if _, ok := contribs[acc.Address]; ok {
			fmt.Println("AiB Addr Duplicate", acc.Address)
		}
	}
	return
}
