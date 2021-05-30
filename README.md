# qftool - a QuickFeather SPI Flash ROM tool

## Overview

The `qftool` is a tool to read and write the flash ROM device on a
[QuickFeather
board](https://github.com/QuickLogic-Corp/quick-feather-dev-board),
written in Go. It has only been tested under Linux.

## Getting started

Build from source:
```
$ go build github.com/tinkerator/qftool
```

When invoked, the `--tty` argument of `qftool` defaults to the
`"/dev/serial/by-id/usb-1d50_6140-if00"` device. It validates that the
SPI ROM identifies as: `MID=0xC8, DID=0x40,0x15`. Anything else causes
the tool to error out.

The QuickFeather board comes with a _bootloader_ that will only allow
remote direct access to the SPI Flash ROM when the bootloader is in
_programming mode_.

After connecting the QuickFeather board to your Linux machine via a
USB cable, to enter _programming mode_ you press the _RESET_ button
(the board's BLUE LED starts flashing), and then press the _USER_
button within the next 5 seconds (the board's BLUE LED stops and the
GREEN LED will start to pulse).

See below for some worked examples, but first a warning:

## Recovering your QuickFeather board

Caution! Using `qftool`, there is a non-zero chance you will corrupt
the SPI Flash ROM on your QuickFeather. Recovery is complicated and
will likely require other tools and/or hardware. This is the summary
of how to bootstrap a QuickFeather from a fully broken state:

- https://forum.quicklogic.com/viewtopic.php?t=29

By default, `qftool` will, however, complain if you attempt to flash
any section of the SPI Flash ROM outside the "application"
sections. So, there is little likelihood you will need to resort to a
full bootstrap recovery unless you invoke `qftool` with a non-default
`--protect` option.

In general, it is not recommended that you consider doing risky things
unless you have a variant of the [SEGGER J-Link Debug
probe](https://www.segger.com/products/debug-probes/j-link/)...

## The Official Open Source support for QuickFeather

The official tools collected together by
[QuickLogic](https://www.quicklogic.com/) for developing against this
board are the `qorc-sdk`. Instructions for downloading and using that
SDK are to be found here:

- https://github.com/QuickLogic-Corp/qorc-sdk

The `qftool` is *not* part of that SDK, but a standalone tool that is
intended to be compatible with binaries generated via that SDK.

## Example usage of `qftool`

While the GREEN LED is pulsing, the board remains in _programming
mode_. All of the following examples assume this to be the case.

### Summarizing the layout of the SPI Flash ROM

To summarize the layout of the SPI Flash ROM on your QuickFeather, do
this:
```
$ ./qftool --layout
```
The first time you do this, you will see an output that looks something like this:
```
$ ./qftool --layout
2021/05/29 18:52:45 section    size/bytes [   base,  limit)    meta={   present     format    purpose}   xcrc32
2021/05/29 18:52:45 ---------- ---------- ----------------- --------    -------    -------    -------  --------
2021/05/29 18:52:45 bootloader 4294967295 [0x00000,0x10000) 0x1f000={     empty    <error>    <error>} FFFFFFFF
2021/05/29 18:52:45 bootfpga        75960 [0x20000,0x40000) 0x10000={   written    <error>    <error>} 6DA74C30
2021/05/29 18:52:45 appfpga    4294967295 [0x40000,0x60000) 0x11000={     empty       fpga        app} FFFFFFFF
2021/05/29 18:52:45 appffe     4294967295 [0x60000,0x80000) 0x12000={     empty    <error>    <error>} FFFFFFFF
2021/05/29 18:52:45 app            112504 [0x80000,0xee000) 0x13000={   written         m4        app} 35C29051
```
Note, the actual `bootloader` only pays attention to the `present` value
of the metadata for each section. Further, it ignores everything
about the configured size and metadata associated with the
`bootloader` itself. This is because the board's bootstrap process
hard-codes loading whatever is in the `bootloader` section of the SPI
Flash ROM and starts executing it.

What makes the QuickFeather board so interesting is the fact that it
has some embedded FPGA functionality. The 'bootfpga' section contains
this data and so long as it is identified as `written` and its
[xcrc32](https://pkg.go.dev/zappem.net/pub/debug/xcrc32) value
correctly summarizes the `size` of bytes from the beginning of this
section, the bootloader will upload it into the FPGA gates to
initialize the bootloader's FPGA support. In the default ROM, this is
a USB to serial interface, and this is needed to be present and
correct in order for `qftool` to work!

The basic boot process is:
- execute `bootloader`
- confirm CRC for `bootfpga` and intall/execute it
- wait 5 seconds for user to press the `USER` button and enter programming mode
  - the board remains in this state until the user presses `RESET`
    again (start over)
- confirm presence and CRC of `appfpga` and if present install/execute it
- confirm presence and CRC of `app` and if present chain-load to it

By default, the `qftool` only allows modifications to the `app*`
sections, so it should ensure the board is always able to get into
_programming mode_ after a `RESET`. (See the Risky operations section
below for non-default possibilities.)

### Verifying the CRC of a section of the SPI Flash ROM

As can be seen in the layout (see the previous section), there are two
sections that have meaningful CRC
([xcrc32](https://pkg.go.dev/zappem.net/pub/debug/xcrc32)) values:
`bootfpga` and `app`. You can confirm these CRCs match the bytes in
these sections with the following commands:
```
$ ./qftool --section=bootfpga --check
read [0x020000,0x0328b7] ................................................. done
2021/05/29 19:03:24 "bootfpga" OK
$ ./qftool --section=app --check
read [0x080000,0x09b777] ................................................. done
2021/05/29 19:03:50 "app" OK
```
Whenever `qftool` is used to write a section, it updates the metadata
appropriately, so eventually, if you try the risky operation of
rewriting the `bootfpga` section, the layout for that section will
come to display:
```
2021/05/29 18:52:45 section    size/bytes [   base,  limit)    meta={   present     format    purpose}   xcrc32
2021/05/29 18:52:45 ---------- ---------- ----------------- --------    -------    -------    -------  --------
 .................
2021/05/29 18:52:45 bootfpga        75960 [0x20000,0x40000) 0x10000={   written       fpga       boot} 6DA74C30
```

### Reading a binary section from the SPI Flash ROM

To read a section of the SPI Flash ROM and display it in ([`xxd
-g1`](https://pkg.go.dev/zappem.net/pub/debug/xxd) format) you can do
the following:
```
$ ./qftool --read=- --section=bootloader
read [0x000000,0x00ffff] ................................................. done
00000000: 00 f0 07 20 c5 63 00 20 8f 31 00 20 79 31 00 20  ... .c. .1. y1. 
00000010: 95 31 00 20 9b 31 00 20 a1 31 00 20 00 00 00 00  .1. .1. .1. ....
00000020: 00 00 00 00 00 00 00 00 00 00 00 00 31 3a 00 20  ............1:. 
00000030: a7 31 00 20 00 00 00 00 f1 3a 00 20 11 5b 00 20  .1. .....:. .[. 
00000040: 29 35 00 20 31 35 00 20 00 00 00 00 51 35 00 20  )5. 15. ....Q5. 
00000050: 45 33 00 20 41 34 00 20 49 34 00 20 ad 31 00 20  E3. A4. I4. .1. 
 .................
0000ffc0: ff ff ff ff ff ff ff ff ff ff ff ff ff ff ff ff  ................
0000ffd0: ff ff ff ff ff ff ff ff ff ff ff ff ff ff ff ff  ................
0000ffe0: ff ff ff ff ff ff ff ff ff ff ff ff ff ff ff ff  ................
0000fff0: ff ff ff ff ff ff ff ff ff ff ff ff ff ff ff ff  ................
```
Note, the `--read=-` special file `-` refers to _stdout_.

As can be seen from the layout above, the metadata for this
`bootloader` section does not specify the length of actual data in
this section so the `qftool` just dumps all of the bytes it contains,
_base_ to _limit_.

To read the contents of a section as a binary to a file, do this:
```
$ ./qftool --read=app.binary --section=app
read [0x080000,0x09b777] ................................................. done
$ ls -l app.binary 
-rw-r--r--. 1 tinkerer tinkerer 112504 May 29 19:39 app.binary
```

### Writing a binary to a section of the SPI Flash ROM

To write the contents of a section from a file, do this:
```
$ ./qftool --write=app.binary --section=app
write [0x080000,0x09bfff] ................................................. done
write [0x013000,0x013fff] .............................................. done
$ ./qftool --check --section=app
read [0x080000,0x09b777] ................................................. done
2021/05/29 19:43:16 "app" OK
```

Writes occur in sequence start to finish. Along the way, the tool
needs to erase a sector before writing bytes, and while this is
happening the dots (`'.'`) are temporarily replaced with `'*'` to
reveal the fact that a sector erase has started.

To boot to the app, press the `RESET` button on the QuickFeather board
and wait 5 seconds for the BLUE LED to stop flashing. Once the app has
loaded, the LED typically goes out. Fire up a serial terminal (the
Arduino Serial monitor works), to observe the application running.

### Risky operations

*Before attempting*, you should review the instructions above about
Recovering your QuickFeather board. Problems executing the `qftool`
using risky operations runs a real risk of causing your board to stop
booting!

The `qftool` permits overwriting all of the SPI Flash ROM all-at-once
or in small increments. For now, we are not documenting these, but
`qftool --help` lists all of the commands supported. The one that is
needed to get you into _risky_ territory is the `--protect` command
line argument.

## License info

The `qftool` program is distributed with the same BSD 3-clause license
as that used by [golang](https://golang.org/LICENSE) itself.

## Reporting bugs and feature requests

The `qftool` has been developed purely out of self-interest and a
curiosity for how the details of the QuickFeather board work. Should
you find a bug or want to suggest a feature addition, please use the
[bug tracker](https://github.com/tinkerator/qftool/issues).
