package route

import (
	"encoding/binary"
	"net"
	"sync"
)

/*
linux 内核早起就用32数组来组织路由表，方便最长匹配，后来为了在很多路由条目的情况下也能有高性能，用trie 数据结构

我现在的使用情况路由条目很少，就有以前的方法比较简单。但是可以稍稍改进，用一个32位的uint32来表示哪个数组槽有路由条目
如果某位置1，表示对应的槽里要路由条目，而不需要每次都从最长掩码到最短掩码遍历一遍，

uint32|masklen32|maskLen31|maskLen30......
设计时，把32掩码的路由表放在routeTable[0]里，31掩码的路由表放在routeTable[2]里，以此类推，这样cpu 预读也好些。
查找路由时，最长匹配原则，先判断uint32的置位情况，以此判断数组哪个槽位有路由条目，没有路由条目的槽位对应置位为0，
有路由条目的槽位对应置位为1.

*/
const (
	maskMaxLen = 32
	first      = 4
	second     = 8
)

type NetWork uint32
type routeTable struct {
	sync.RWMutex
	slotMask uint32 //如何根据rtHash 的是否有路由条目来设置 slotMask, 加锁？
	rts      [maskMaxLen]rtEntry
}

type rtEntry struct {
	sync.RWMutex
	mask   uint32
	rtHash map[NetWork]interface{}
}

func NewRouteTable() *routeTable {
	rt := new(routeTable)
	for i := 0; i < maskMaxLen; i++ {
		rt.rts[i].mask = (1 << (maskMaxLen - uint32(i))) - 1
		rt.rts[i].rtHash = make(map[NetWork]interface{})
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
	rt.rts[slot].Lock()
	n := len(rt.rts[slot].rtHash)
	rt.rts[slot].rtHash[net] = v
	rt.rts[slot].Unlock()

	//if there are route entry before add, don't need to set slotMask
	if n > 0 {
		return nil
	}

	//how to set slotMask atomic ??
	rt.Lock()
	if len(rt.rts[slot].rtHash) > 0 {
		rt.slotMask |= 1 << uint32(slot) //set bit
	}
	rt.Unlock()
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
	rt.rts[slot].Lock()
	delete(rt.rts[slot].rtHash, net)
	rt.rts[slot].Unlock()

	//how to set slotMask atomic ??
	rt.Lock()
	if len(rt.rts[slot].rtHash) == 0 {
		rt.slotMask &= ^(1 << uint32(slot)) //clear bit
	}
	rt.Unlock()
	return nil
}

func (rt *routeTable) RouteLookup(ip NetWork) interface{} {
	rt.RLock()
	rtMask := rt.slotMask
	rt.RUnlock()
	for i := 0; i < maskMaxLen; i++ {
		//maybe:don't have to iter maskMaxLen times
		if rtMask == 0 {
			break
		}

		if rtMask&1 != 0 {
			net := ip & NetWork(rt.rts[i].mask)
			rt.rts[i].RLock()
			if v, ok := rt.rts[i].rtHash[net]; ok {
				rt.rts[i].RUnlock()
				return v
			}
			rt.rts[i].RUnlock()
		}

		rtMask >>= 1
	}
	return nil
}
