# Example `goal` Snippets

Unless otherwise noted, it is assumed that the working directory
begins at the top level of the `go-algorand` repo.

It is also assumed that the main README's installation instructions have been followed and `make install` run so that the `goal` executable has been rebuilt from the same source as this example and is available in the shell environment.
You can run `ls -l $(which goal)` after your `make install` and look at the installation time as a sanity check.

Finally, all the `goal` commands assume that `${DTALGORAND_DATA}` has been set. See the first Q/A for how this is done.

## Starting a Single Node Dev Network

### Q: Having just completed a new build in go-algorand, how do I get a single node dev network up, with algos in an easily accessible wallet from goal?

### A:

Set `NETWORKS` to where you want to keep the network files (the data directories go beneath it), create the directory if you don't have it yet, and set `NAME` to your network's name:

```sh
NETWORKS=~/networks
mkdir -p ${NETWORKS}
NAME=niftynetwork
```

Create the network, from the root directory of the go-algorand repo:

```sh
goal network create -n ${NAME} -r ${NETWORKS}/${NAME} -t ./test/testdata/nettemplates/OneNodeFuture.json
```

Set `DTALGORAND_DATA`, which the rest of this README assumes is set, and start the network:

```sh
export DTALGORAND_DATA=${NETWORKS}/${NAME}/Primary
echo $DTALGORAND_DATA
goal node start
```

Check that it works: the block number goes up from one status to the next. The `sleep`s are there so that you can paste the whole block at once; otherwise run `goal node status` a few times.

```sh
goal node status
sleep 4
goal node status
sleep 4
goal node status
```

Find the account with all the money, put it in a variable, and send some money from the account to itself:

```sh
goal account list
ACCOUNT=`goal account list | awk '{print $2}'`
echo $ACCOUNT
goal clerk send --to ${ACCOUNT} --from ${ACCOUNT} --amount 10
```

## Creating Applications

### Q: How do I use goal to create an app?

### A:
Here's an example with the following assumptions:
* all the setup is as in the first question
* the approval program (which tests box functionality) has relative path `cmd/goal/examples/boxes.teal`
* the clear program has relative path `cmd/goal/examples/clear.teal`
* there are no local or global storage requirements

Create the app, and take note of its "app index":

```sh
TEALDIR=cmd/goal/examples
echo $TEALDIR
goal app create --creator ${ACCOUNT} --approval-prog ${TEALDIR}/boxes.teal --clear-prog ${TEALDIR}/clear.teal
```

For the following questions, you'll need to use the app index. That will be shown in the last line printed. EG:

```sh
Attempting to create app (approval size 125, hash RKWO3VXBKQXF77PC6EHRLFXD4YTJYTJTGPTPWQ46YH5ESGPZ5JIA; clear size 3, hash IS4FW6ZCRMQRTDIINAVAQHD2GK6DXUNQHQ52IQGZEVPP4OEU56QA)
Issued transaction from account ECRQFXZ7P3PLNK6QLIEVX7AXU6NTVQZHFUSEXTXMBKKOA2NTIV4PCX7XNY, txid SZK3U7AARMPQSZUICZIGYRLC7UDXJCVPV34JCBN5LIBXMF635UKA (fee 1000)
Transaction SZK3U7AARMPQSZUICZIGYRLC7UDXJCVPV34JCBN5LIBXMF635UKA still pending as of round 12
Transaction SZK3U7AARMPQSZUICZIGYRLC7UDXJCVPV34JCBN5LIBXMF635UKA still pending as of round 13
Transaction SZK3U7AARMPQSZUICZIGYRLC7UDXJCVPV34JCBN5LIBXMF635UKA committed in round 14
Created app with app index 2
```

## Funding App-Accounts

### Q: How do I fund the app account so that it can satisfy its boxes min-balance requirement and allow for box creation?

### A:
Assuming you followed the previous step, and that the _app index_ is 2:

Store the app index and the app's account address in variables for later use:

```sh
APPID=2
echo $APPID
APP_ACCOUNT=`goal app info --app-id ${APPID} | grep "Application account" | awk '{print $3}'`
echo $APP_ACCOUNT
```

Fund the app's account (here we're being very conservative and sending 10 algos), and check its balance:

```sh
goal clerk send --to ${APP_ACCOUNT} --from ${ACCOUNT} --amount 10000000
goal account balance --address ${APP_ACCOUNT}
```

## Application Boxes in `goal`

### Q: How do I use boxes in goal? In particular, I'd like to make a goal app call which:
* accesses a particular box for a particular app
* stores an ABI type as its contents

### A:
Here's an example with the following assumptions:

* the caller's account is given by `${ACCOUNT}` (see first answer)
* the program used is `boxes.teal` referenced above. In particular:
  * it routes to box subroutines using the app argument at index 0 as the method signifier
* the app id has been stored in `${APPID}` (see the previous answer)
* the box referenced in the first non-create app-call has name `greatBox`
* another referenced box is named `an_ABI_box`
  * this second box is provided contents `[2,3,5]` of ABI-type `(uint8,uint8,uint8)`

Create a box with a simple non-ABI name. Note how the `--box` flag needs to be set so as to refer to the box being touched:

```sh
goal app call --from $ACCOUNT --app-id ${APPID} --box "str:greatBox" --app-arg "str:create" --app-arg "str:greatBox"
```

Create another box:

```sh
goal app call --from ${ACCOUNT} --app-id ${APPID} --box "str:an_ABI_box" --app-arg "str:create" --app-arg "str:an_ABI_box"
```

Set its contents to ABI type `(uint8,uint8,uint8)` with value `[2,3,5]`:

```sh
goal app call --from ${ACCOUNT} --app-id ${APPID} --box "str:an_ABI_box" --app-arg "str:set" --app-arg "str:an_ABI_box"  --app-arg "abi:(uint8,uint8,uint8):[2,3,5]"
```

### Q: How do I search for boxes in goal?

### A:
Assuming you followed the previous step to create `greatBox` and `an_ABI_box`:

Get all boxes for a given app:

```sh
goal app box list --app-id ${APPID}
```

Get the box details for a given box:

```sh
goal app box info --app-id ${APPID} --name "str:an_ABI_box"
```

## Past Rounds and Versions (time travel)

### Q: My node runs with `EnableTimeTravel`. How do I see an account, app or box as it was at an earlier round, or how it changed?

### A:
The read commands take `--round`, and `versions` subcommands list the rounds at which an object changed, with the fields that changed:

The box and the app as of round 1000:

```sh
goal app box info --app-id ${APPID} --name "str:greatBox" --round 1000
goal app info --app-id ${APPID} --round 1000
```

Every change of the box; of every box of the app since round 1000 (no `--name`); of the app's params and global state; and of an account:

```sh
goal app box versions --app-id ${APPID} --name "str:greatBox"
goal app box versions --app-id ${APPID} --min-round 1000
goal app versions --app-id ${APPID} --min-round 1000 --all
goal account versions --address ${ACCOUNT} --exclude all
```

The raw pages, as JSON:

```sh
goal asset versions --assetid ${ASSETID} --json
```

The holders of an asset as of round 1000: address, amount, "frozen" (needs `TimeTravelHoldersIndex`):

```sh
goal asset holders --assetid ${ASSETID} --round 1000 --all
```

`--round 0` is the genesis round, except for `goal app box list`, where `--round 0` means the latest round. A node without time travel rejects `--round`. To explore the history of a stopped node offline, including the raw records, see `tools/ttdump`.
