package routev2

import (
	"encoding/binary"
	"net"
	"sync"
)

const (
	maskMaxLen  = 32
	IpSection   = 4
	SectionSize = 8
)

/*
linux 内核早起就用32数组来组织路由表，方便最长匹配，后来为了在很多路由条目的情况下也能有高性能，用trie 数据结构

还有dpdk 实现的lmp 最长匹配算法，它主要是用空间换时间，换取更高的查找效率：路由表分两段存储，前缀24之间用2^24大小
的数组来表示，剩下的8位，用2^8的数组了存储，当然，如果2^8的数组根据需要临时分配，如果都是路由表的前缀都小于24,那么
2^8的数组是不需要分配的，而且在查找路由的时候，O(1) 时间复杂度就可以找到匹配到，如果路由表的前缀都大于24，最多也是
两次内存读取就能匹配到相应的路由。

我现在的使用情况路由条目很少，就有以前的方法比较简单。但是可以稍稍改进，用一个32位的uint32来表示哪个数组槽有路由条目
如果某位置1，表示对应的槽里要路由条目，而不需要每次都从最长掩码到最短掩码遍历一遍，

uint32|masklen32|maskLen31|maskLen30......
设计时，把32掩码的路由表放在routeTable[0]里，31掩码的路由表放在routeTable[2]里，以此类推，这样cpu 预读也好些。
查找路由时，最长匹配原则，先判断uint32的置位情况，以此判断数组哪个槽位有路由条目，没有路由条目的槽位对应置位为0，
有路由条目的槽位对应置位为1.

或者干脆记录有掩码长度,但是掩码长度大的在路由表放到前面的槽里，避免每次都检测槽里有没有路由.
[0]{maskLen:24, rtNum:xxx ,rtHash:xxx, rtAarry:xxx}
[1]{maskLen:22, rtNum:xxx ,rtHash:xxx, rtAarry:xxx}
[2]{maskLen:16, rtNum:xxx ,rtHash:xxx, rtAarry:xxx}
[3]{maskLen:0, rtNum:xxx ,rtHash:xxx, rtAarry:xxx}
如果路由条目的数量rtNum 小于某个数值(比如4)那么就把路由条目放在rtAarry数组，如果rtNum超过一定的数值，就用哈希表rtHash来存储路由条目

还可以分段, 锁颗粒度变小，锁的使用也清晰。下面就是相关实现：
*/
type NetWork uint32
type routeTable struct {
	rts [IpSection]rtSection
}

type rtSection struct {
	sync.RWMutex
	slotMask uint8 //SectionSize bit
	rtSec    [SectionSize]rtEntry
}

type rtEntry struct {
	mask   uint32
	rtHash map[NetWork]interface{}
}

func NewRouteTable() *routeTable {
	rt := new(routeTable)
	idx := 0
	for i := 0; i < IpSection; i++ {
		for j := 0; j < SectionSize; j++ {
			idx = i*SectionSize + j
			section := &rt.rts[i]
			section.rtSec[j].mask = (1 << (maskMaxLen - uint32(idx))) - 1
			section.rtSec[j].rtHash = make(map[NetWork]interface{})
		}
	}
	return rt
}

func (rt *routeTable) AddRoute(network string, v interface{}) error {
	_, ipnet, err := net.ParseCIDR(network)
	if err != nil {
		return err
	}

	maskLen, _ := ipnet.Mask.Size()
	slot := maskMaxLen - maskLen
	net := NetWork(binary.BigEndian.Uint32(ipnet.IP))

	ipID := slot / IpSection
	secID := slot & (SectionSize - 1)
	sec := &rt.rts[ipID]
	rte := &sec.rtSec[secID]

	sec.Lock()
	rte.rtHash[net] = v
	sec.slotMask |= 1 << uint8(secID)
	sec.Unlock()
	return nil
}

func (rt *routeTable) DelRoute(network string) error {
	_, ipnet, err := net.ParseCIDR(network)
	if err != nil {
		return err
	}

	maskLen, _ := ipnet.Mask.Size()
	slot := maskMaxLen - maskLen
	net := NetWork(binary.BigEndian.Uint32(ipnet.IP))

	ipID := slot / IpSection
	secID := slot & (SectionSize - 1)
	sec := &rt.rts[ipID]
	rte := &sec.rtSec[secID]

	sec.Lock()
	delete(rte.rtHash, net)
	if len(rte.rtHash) == 0 {
		sec.slotMask &= ^(1 << uint8(secID)) //clear bit
	}
	sec.Unlock()
	return nil
}

func (rt *routeTable) RouteLookup(ip NetWork) interface{} {
	var sec *rtSection
	var rte *rtEntry
	var net NetWork
	var bitMask uint8
	for i := 0; i < IpSection; i++ {
		sec = &rt.rts[i]
		sec.RLock()
		bitMask = sec.slotMask
		sec.Unlock()
		for j := 0; j < SectionSize; j++ {
			if bitMask == 0 {
				break
			}
			if bitMask&1 != 0 {
				rte = &sec.rtSec[j]
				net = NetWork(rte.mask) & ip
				if v, ok := rte.rtHash[net]; ok {
					return v
				}
			}
			bitMask >>= 1
		}
	}
	return nil
}
