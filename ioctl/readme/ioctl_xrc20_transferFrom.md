## ioctl xrc20 transferFrom

Send amount of tokens from owner address to target address

### Synopsis

Send amount of tokens from owner address to target address

```
ioctl xrc20 transferFrom (ALIAS|OWNER_ADDRESS) (ALIAS|RECIPIENT_ADDRESS) AMOUNT -c (ALIAS|CONTRACT_ADDRESS) [-s SIGNER] [-n NONCE] [-l GAS_LIMIT] [-p GAS_PRICE] [-P PASSWORD] [-y] [flags]
```

### Options

```
  -y, --assume-yes          answer yes for all confirmations
  -l, --gas-limit uint     set gas limit
  -p, --gas-price string   set gas price (unit: 10^(-6)IOTX), use suggested gas price if input is "0" (default "1")
  -h, --help               help for transferFrom
  -n, --nonce uint         set nonce (default using pending nonce)
  -P, --password string    input password for account
  -s, --signer string      choose a signing account
```

### Options inherited from parent commands

```
  -c, --contract-address string   set contract address
      --endpoint string           set endpoint for once (default "api.iotex.one:443")
      --insecure                  insecure connection for once (default false)
  -o, --output-format string      output format
```

### SEE ALSO

* [ioctl xrc20](ioctl_xrc20.md)	 - Support ERC20 standard command-line from ioctl

###### Auto generated by ioctl on 27-Nov-2019
