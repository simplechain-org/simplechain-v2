package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/leveldb"
)

type probeDB interface {
	ethdb.Reader
	ethdb.Iteratee
}

func main() {
	var (
		datadir   = flag.String("datadir", "", "node datadir root, e.g. /data/node-deploy/.local/node0")
		num       = flag.Uint64("num", 0, "block number to inspect")
		hashHex   = flag.String("hash", "", "block hash to inspect")
		walkBack  = flag.Int("walk-back", 0, "walk backward N parents from the selected block")
		allHashes = flag.Bool("all-hashes", false, "print all known hashes for --num")
	)
	flag.Parse()

	if *datadir == "" {
		fmt.Fprintln(os.Stderr, "missing --datadir")
		os.Exit(2)
	}

	chaindata := filepath.Join(*datadir, "geth", "chaindata")
	ancient := filepath.Join(chaindata, "ancient")

	ldb, err := leveldb.New(chaindata, 128, 256, "dbprobe/", true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open leveldb: %v\n", err)
		os.Exit(1)
	}
	defer ldb.Close()

	db, err := rawdb.NewDatabaseWithFreezer(ldb, ancient, "dbprobe/", true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open freezer db: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	printHead(db)

	if *num > 0 || *allHashes {
		printByNumber(db, *num, *allHashes)
	}
	if *hashHex != "" {
		hash := common.HexToHash(*hashHex)
		printByHash(db, hash)
		if *walkBack > 0 {
			walkParents(db, hash, *walkBack)
		}
		return
	}
	if *num > 0 && *walkBack > 0 {
		hash := rawdb.ReadCanonicalHash(db, *num)
		if hash == (common.Hash{}) {
			hashes := rawdb.ReadAllHashes(db, *num)
			if len(hashes) == 1 {
				hash = hashes[0]
			} else {
				fmt.Fprintf(os.Stderr, "cannot walk from number %d: canonical missing, candidates=%d\n", *num, len(hashes))
				os.Exit(3)
			}
		}
		walkParents(db, hash, *walkBack)
	}
}

func printHead(db probeDB) {
	headBlockHash := rawdb.ReadHeadBlockHash(db)
	headHeaderHash := rawdb.ReadHeadHeaderHash(db)
	headFastHash := rawdb.ReadHeadFastBlockHash(db)
	finalized := rawdb.ReadFinalizedBlockHash(db)
	safe := rawdb.ReadLastPivotNumber(db)

	fmt.Printf("head.block=%s", headBlockHash.Hex())
	if n := rawdb.ReadHeaderNumber(db, headBlockHash); n != nil {
		fmt.Printf(" number=%d", *n)
	}
	fmt.Println()

	fmt.Printf("head.header=%s", headHeaderHash.Hex())
	if n := rawdb.ReadHeaderNumber(db, headHeaderHash); n != nil {
		fmt.Printf(" number=%d", *n)
	}
	fmt.Println()

	fmt.Printf("head.fast=%s", headFastHash.Hex())
	if n := rawdb.ReadHeaderNumber(db, headFastHash); n != nil {
		fmt.Printf(" number=%d", *n)
	}
	fmt.Println()

	fmt.Printf("head.finalized=%s", finalized.Hex())
	if n := rawdb.ReadHeaderNumber(db, finalized); n != nil {
		fmt.Printf(" number=%d", *n)
	}
	fmt.Println()

	if safe != nil {
		fmt.Printf("lastPivot=%d\n", *safe)
	}
}

func printByNumber(db probeDB, num uint64, showAll bool) {
	hash := rawdb.ReadCanonicalHash(db, num)
	if hash == (common.Hash{}) {
		fmt.Printf("canonical[%d]=<missing>\n", num)
	} else {
		fmt.Printf("canonical[%d]=%s\n", num, hash.Hex())
		printBlockLine(db, hash, num)
	}
	if showAll {
		hashes := rawdb.ReadAllHashes(db, num)
		fmt.Printf("allHashes[%d]=%d\n", num, len(hashes))
		for _, h := range hashes {
			printBlockLine(db, h, num)
		}
	}
}

func printByHash(db probeDB, hash common.Hash) {
	num := rawdb.ReadHeaderNumber(db, hash)
	if num == nil {
		fmt.Printf("hash=%s number=<missing>\n", hash.Hex())
		return
	}
	printBlockLine(db, hash, *num)
}

func printBlockLine(db probeDB, hash common.Hash, num uint64) {
	header := rawdb.ReadHeader(db, hash, num)
	body := rawdb.ReadBody(db, hash, num)
	isCanon := rawdb.ReadCanonicalHash(db, num) == hash
	if header == nil {
		fmt.Printf("block num=%d hash=%s canonical=%t header=<missing> body=%t\n", num, hash.Hex(), isCanon, body != nil)
		return
	}
	txCount := 0
	if body != nil {
		txCount = len(body.Transactions)
	}
	fmt.Printf("block num=%d hash=%s canonical=%t body=%t parent=%s root=%s time=%d txs=%d\n",
		num, hash.Hex(), isCanon, body != nil, header.ParentHash.Hex(), header.Root.Hex(), header.Time, txCount)
}

func walkParents(db probeDB, start common.Hash, steps int) {
	hash := start
	for i := 0; i <= steps; i++ {
		num := rawdb.ReadHeaderNumber(db, hash)
		if num == nil {
			fmt.Printf("walk step=%d hash=%s number=<missing>\n", i, hash.Hex())
			return
		}
		header := rawdb.ReadHeader(db, hash, *num)
		body := rawdb.ReadBody(db, hash, *num)
		if header == nil {
			fmt.Printf("walk step=%d num=%d hash=%s header=<missing> body=%t\n", i, *num, hash.Hex(), body != nil)
			return
		}
		fmt.Printf("walk step=%d num=%d hash=%s canonical=%t body=%t parent=%s root=%s\n",
			i, *num, hash.Hex(), rawdb.ReadCanonicalHash(db, *num) == hash, body != nil, header.ParentHash.Hex(), header.Root.Hex())
		if *num == 0 {
			return
		}
		hash = header.ParentHash
	}
}
