// Copyright (C) 2019-2026 Algorand Foundation Ltd.
// This file is part of go-algorand
//
// go-algorand is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// go-algorand is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with go-algorand.  If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/daemon/algod/api/client"
	v2 "github.com/algorand/go-algorand/daemon/algod/api/server/v2"
	"github.com/algorand/go-algorand/data/account"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/transactions/logic"
	"github.com/algorand/go-algorand/data/txntest"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/util/db"
)

// demo drives a running dev-mode time travel node (no kmd needed): it builds
// history signed with the network's root keys, recording the live state of
// every round, then asks the REST API about old rounds and compares.
func demo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	netDir := fs.String("net", "", "network root directory (created with goal network create -K)")
	rounds := fs.Int("rounds", 1150, "history to build")
	fs.Parse(args)

	dataDir := filepath.Join(*netDir, "Node")
	addr, err := os.ReadFile(filepath.Join(dataDir, "algod.net"))
	if err != nil {
		return err
	}
	token, err := os.ReadFile(filepath.Join(dataDir, "algod.token"))
	if err != nil {
		return err
	}
	base := "http://" + strings.TrimSpace(string(addr))
	u, _ := url.Parse(base)
	c := client.MakeRestClient(*u, strings.TrimSpace(string(token)))
	get := func(path string) (int, map[string]any) {
		req, _ := http.NewRequest(http.MethodGet, base+path, nil)
		req.Header.Set("X-Algo-API-Token", strings.TrimSpace(string(token)))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, nil
		}
		defer resp.Body.Close()
		var m map[string]any
		json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}

	var keys []*crypto.SignatureSecrets
	for _, w := range []string{"Wallet1", "Wallet2"} {
		acc, err := db.MakeAccessor(filepath.Join(*netDir, w+".rootkey"), true, false)
		if err != nil {
			return err
		}
		root, err := account.RestoreRoot(acc)
		acc.Close()
		if err != nil {
			return err
		}
		keys = append(keys, root.Secrets())
	}
	a, p := basics.Address(keys[0].SignatureVerifier), basics.Address(keys[1].SignatureVerifier)

	sp, err := c.SuggestedParams()
	if err != nil {
		return err
	}
	var gh crypto.Digest
	copy(gh[:], sp.GenesisHash)
	proto := config.Consensus[protocol.ConsensusVersion(sp.ConsensusVersion)]
	send := func(tx txntest.Txn) (transactions.SignedTxn, error) {
		st, err := c.Status()
		if err != nil {
			return transactions.SignedTxn{}, err
		}
		tx.FirstValid, tx.LastValid, tx.GenesisHash, tx.GenesisID = st.LastRound, st.LastRound+1000, gh, sp.GenesisId
		tx.FillDefaults(proto)
		stxn := tx.Txn().Sign(keys[0])
		_, err = c.SendRawTransaction(stxn)
		return stxn, err
	}

	approval, err := logic.AssembleString(`#pragma version 10
txn ApplicationID
bz done
byte "n"; app_global_get; itob; log
byte "b"; box_get; pop; log
addr ` + p.String() + `; balance; itob; log
byte "n"; byte "n"; app_global_get; int 1; +; app_global_put
byte "b"; txn ApplicationArgs 0; box_put
done: int 1`)
	if err != nil {
		return err
	}
	clear, _ := logic.AssembleString("#pragma version 10\nint 1")
	create, err := send(txntest.Txn{Type: protocol.ApplicationCallTx, Sender: a, ApprovalProgram: approval.Program, ClearStateProgram: clear.Program,
		GlobalStateSchema: basics.StateSchema{NumUint: 1}})
	if err != nil {
		return err
	}
	pend, err := c.PendingTransactionInformation(create.ID().String())
	if err != nil || pend.ApplicationIndex == nil {
		return fmt.Errorf("app create: %v", err)
	}
	app := *pend.ApplicationIndex
	if _, err := send(txntest.Txn{Type: protocol.PaymentTx, Sender: a, Receiver: app.Address(), Amount: 10_000_000}); err != nil {
		return err
	}

	type live struct {
		n, bal float64
		raw    float64 // the balance without pending rewards: changes only with the account record
		box    string
	}
	history := map[uint64]live{}
	var first uint64
	for i := 0; i < *rounds; i++ {
		if i%2 == 0 {
			_, err = send(txntest.Txn{Type: protocol.ApplicationCallTx, Sender: a, ApplicationID: app, Accounts: []basics.Address{p},
				ApplicationArgs: [][]byte{binary.BigEndian.AppendUint64(nil, uint64(i))}, Boxes: []transactions.BoxRef{{Name: []byte("b")}}})
		} else {
			tx := txntest.Txn{Type: protocol.PaymentTx, Sender: a, Receiver: p, Amount: uint64(1000 + i)}
			_, err = send(tx)
		}
		if err != nil {
			return err
		}
		_, appInfo := get(fmt.Sprintf("/v2/applications/%d", app))
		_, box := get(fmt.Sprintf("/v2/applications/%d/box?name=str:b", app))
		_, acct := get("/v2/accounts/" + p.String() + "?exclude=all")
		rnd := uint64(acct["round"].(float64))
		if first == 0 {
			first = rnd
		}
		history[rnd] = live{n: globalUint(appInfo), box: fmt.Sprint(box["value"]), bal: acct["amount"].(float64), raw: acct["amount-without-pending-rewards"].(float64)}
	}
	st, _ := c.Status()
	fmt.Printf("built rounds %d..%d (latest %d)\n", first, st.LastRound, st.LastRound)

	failed := 0
	check := func(what string, ok bool) {
		mark := "ok  "
		if !ok {
			mark = "FAIL"
			failed++
		}
		fmt.Printf("  [%s] %s\n", mark, what)
	}
	for _, r := range []uint64{first, first + 1, first + 101} {
		want := history[r]
		fmt.Printf("round %d (live then: n=%.0f box=%s balance=%.0f)\n", r, want.n, want.box, want.bal)
		code, _ := get(fmt.Sprintf("/v2/blocks/%d", r))
		check(fmt.Sprintf("GET /v2/blocks/%d -> %d (block body pruned)", r, code), code == http.StatusNotFound)
		code, blk := get(fmt.Sprintf("/v2/blocks/%d?header-only=true", r))
		check(fmt.Sprintf("GET /v2/blocks/%d?header-only=true -> %d", r, code), code == http.StatusOK && blk["block"] != nil)
		code, appInfo := get(fmt.Sprintf("/v2/applications/%d?round=%d", app, r))
		check(fmt.Sprintf("GET /v2/applications/%d?round=%d -> %d n=%.0f", app, r, code, globalUint(appInfo)), code == http.StatusOK && globalUint(appInfo) == want.n)
		code, box := get(fmt.Sprintf("/v2/applications/%d/box?name=str:b&round=%d", app, r))
		check(fmt.Sprintf("GET /v2/applications/%d/box?name=str:b&round=%d -> %d value=%v", app, r, code, box["value"]), code == http.StatusOK && fmt.Sprint(box["value"]) == want.box)
		code, acct := get(fmt.Sprintf("/v2/accounts/%s?round=%d", p, r))
		check(fmt.Sprintf("GET /v2/accounts/<P>?round=%d -> %d amount=%v round=%v", r, code, acct["amount"], acct["round"]), code == http.StatusOK && acct["amount"] == want.bal && acct["round"] == float64(r))
		code, acct = get(fmt.Sprintf("/v2/accounts/%s?round=%d", a, r))
		check(fmt.Sprintf("GET /v2/accounts/<creator>?round=%d -> %d created-apps=%d", r, code, len(asSlice(acct["created-apps"]))), code == http.StatusOK && len(asSlice(acct["created-apps"])) == 1)
		code, supply := get(fmt.Sprintf("/v2/ledger/supply?round=%d", r))
		check(fmt.Sprintf("GET /v2/ledger/supply?round=%d -> %d current_round=%v", r, code, supply["current_round"]), code == http.StatusOK && supply["current_round"] == float64(r))

		// simulate after round r: same app, no box/account references (unnamed resources)
		tx := txntest.Txn{Type: protocol.ApplicationCallTx, Sender: a, ApplicationID: app, ApplicationArgs: [][]byte{{9}},
			FirstValid: basics.Round(r), LastValid: basics.Round(r) + 10, GenesisHash: gh, GenesisID: sp.GenesisId}
		tx.FillDefaults(proto)
		body := protocol.EncodeReflect(&v2.PreEncodedSimulateRequest{
			TxnGroups: []v2.PreEncodedSimulateRequestTransactionGroup{{Txns: []transactions.SignedTxn{tx.Txn().Sign(keys[0])}}},
			Round:     basics.Round(r), AllowUnnamedResources: true})
		req, _ := http.NewRequest(http.MethodPost, base+"/v2/transactions/simulate?format=json", bytes.NewReader(body))
		req.Header.Set("X-Algo-API-Token", strings.TrimSpace(string(token)))
		req.Header.Set("Content-Type", "application/msgpack")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var sim struct {
			TxnGroups []struct {
				FailureMessage string `json:"failure-message"`
				TxnResults     []struct {
					TxnResult struct {
						Logs []string `json:"logs"`
					} `json:"txn-result"`
				} `json:"txn-results"`
			} `json:"txn-groups"`
		}
		json.Unmarshal(raw, &sim)
		var logs []string
		if len(sim.TxnGroups) > 0 && len(sim.TxnGroups[0].TxnResults) > 0 {
			logs = sim.TxnGroups[0].TxnResults[0].TxnResult.Logs
		}
		wantLogs := []string{b64u(uint64(want.n)), want.box, b64u(uint64(want.bal))}
		check(fmt.Sprintf("POST /v2/transactions/simulate {round:%d, allow-unnamed-resources} -> %d logs=%v", r, resp.StatusCode, logs),
			resp.StatusCode == http.StatusOK && fmt.Sprint(logs) == fmt.Sprint(wantLogs))
	}

	// versions: the rounds at which the live values changed, paged
	lo, hi := first+1, first+40
	changed := func(f func(live) any) (rounds []uint64) {
		for r := lo; r <= hi; r++ {
			if f(history[r]) != f(history[r-1]) {
				rounds = append(rounds, r)
			}
		}
		return rounds
	}
	pages := func(path string) (versions []map[string]any, n int) {
		for next := ""; ; n++ {
			code, resp := get(path + next)
			if code != http.StatusOK {
				return nil, n
			}
			for _, v := range asSlice(resp["versions"]) {
				versions = append(versions, v.(map[string]any))
			}
			tok, ok := resp["next-token"].(string)
			if !ok {
				return versions, n + 1
			}
			next = "&next=" + tok
		}
	}
	roundsOf := func(vs []map[string]any) (out []uint64) {
		for _, v := range vs {
			out = append(out, uint64(v["round"].(float64)))
		}
		return out
	}
	span := fmt.Sprintf("min-round=%d&max-round=%d", lo, hi)
	fmt.Printf("versions in rounds %d..%d\n", lo, hi)

	want := changed(func(l live) any { return l.n })
	vs, n := pages(fmt.Sprintf("/v2/applications/%d/versions?%s&limit=7&include=values", app, span))
	valuesOK := len(vs) == len(want)
	for _, v := range vs {
		valuesOK = valuesOK && globalUint(v["application"].(map[string]any)) == history[uint64(v["round"].(float64))].n
	}
	check(fmt.Sprintf("GET /v2/applications/%d/versions?include=values, %d pages -> rounds %v", app, n, roundsOf(vs)),
		fmt.Sprint(roundsOf(vs)) == fmt.Sprint(want) && valuesOK)
	desc, n := pages(fmt.Sprintf("/v2/applications/%d/versions?%s&limit=3&order=desc", app, span))
	reversed := roundsOf(desc)
	slices.Reverse(reversed)
	check(fmt.Sprintf("GET /v2/applications/%d/versions?order=desc, %d pages -> the same rounds, newest first", app, n),
		fmt.Sprint(reversed) == fmt.Sprint(want))

	want = changed(func(l live) any { return l.box })
	vs, _ = pages(fmt.Sprintf("/v2/applications/%d/box/versions?name=str:b&%s&include=values", app, span))
	valuesOK = len(vs) == len(want)
	for _, v := range vs {
		valuesOK = valuesOK && fmt.Sprint(v["value"]) == history[uint64(v["round"].(float64))].box
	}
	check(fmt.Sprintf("GET /v2/applications/%d/box/versions?name=str:b&include=values -> %d versions", app, len(vs)),
		fmt.Sprint(roundsOf(vs)) == fmt.Sprint(want) && valuesOK)

	// pending rewards move amount every round; versions follow the record
	want = changed(func(l live) any { return l.raw })
	vs, _ = pages(fmt.Sprintf("/v2/accounts/%s/versions?exclude=all&%s&include=values&limit=5", p, span))
	valuesOK = len(vs) == len(want)
	for _, v := range vs {
		base, _ := v["base"].(map[string]any)
		acct, _ := base["account"].(map[string]any)
		valuesOK = valuesOK && acct["amount"] == history[uint64(v["round"].(float64))].bal
	}
	check(fmt.Sprintf("GET /v2/accounts/<P>/versions?exclude=all&include=values -> %d versions", len(vs)),
		fmt.Sprint(roundsOf(vs)) == fmt.Sprint(want) && valuesOK)

	// the creator pays a fee every round, and its app changes on every call
	vs, _ = pages(fmt.Sprintf("/v2/accounts/%s/versions?%s", a, span))
	appRounds := 0
	for _, v := range vs {
		if len(asSlice(v["created-apps"])) == 1 {
			appRounds++
		}
	}
	check(fmt.Sprintf("GET /v2/accounts/<creator>/versions -> %d versions, %d with created-apps", len(vs), appRounds),
		len(vs) == int(hi-lo+1) && appRounds == len(changed(func(l live) any { return l.n })))
	if failed > 0 {
		return fmt.Errorf("%d checks failed", failed)
	}
	return nil
}

func b64u(v uint64) string {
	return base64.StdEncoding.EncodeToString(binary.BigEndian.AppendUint64(nil, v))
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func globalUint(appInfo map[string]any) float64 {
	params, _ := appInfo["params"].(map[string]any)
	for _, kv := range asSlice(params["global-state"]) {
		m := kv.(map[string]any)
		if m["key"] == base64.StdEncoding.EncodeToString([]byte("n")) {
			v, _ := m["value"].(map[string]any)
			u, _ := v["uint"].(float64)
			return u
		}
	}
	return 0
}
