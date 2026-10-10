package lxgo

// builtinSearchScript 是一个内置的"只做搜索"的兜底音源。
//
// 背景：用户放到 js/ 目录下的音源脚本（HYWmusic、玉宁熙、星海、墨澜等）绝大多数只
// 实现了 musicUrl（取直链），并不实现 search（搜索）—— 这是真实 lx-music 生态里的
// 常态，搜索通常交给客户端自带的搜索接口，第三方音源脚本只负责"拿到 musicInfo 之后
// 怎么解析出直链"。但 pkg/music 这边的使用场景是"先搜索拿到 musicInfo，再取直链"，
// 如果一个搜索能力都没有，GET /api/music/search 会一直 404。
//
// 所以这里内置一个轻量搜索源，覆盖 wy/kw/mg 三个平台（覆盖面对齐 pkg/music 默认配置
// 的 source: "wy"），始终追加在 js/ 目录加载的用户脚本之后（优先级最低）——如果用户
// 自己放的某个脚本以后也实现了 search，会优先用用户的。
const builtinSearchScript = `
/*!
 * @name 内置搜索源
 * @description lx-go 内置的搜索兜底音源，只实现 search，不做 musicUrl（配合其它音源使用）
 * @version 1.0.0
 */
const { EVENT_NAMES, request, on, send } = globalThis.lx

const httpGet = (url, options = {}) => new Promise((resolve, reject) => {
  request(url, { method: 'GET', timeout: 8000, ...options }, (err, resp) => {
    if (err) return reject(err)
    try { resolve(JSON.parse(resp.body)) } catch (e) { reject(new Error('响应不是合法 JSON')) }
  })
})

const formatDuration = (sec) => {
  sec = Math.floor(Number(sec) || 0)
  const m = Math.floor(sec / 60)
  const s = sec % 60
  return String(m).padStart(2, '0') + ':' + String(s).padStart(2, '0')
}

async function searchChksz(keyword, limit) {
  const body = await httpGet('https://api.chksz.top/api/163_search?keyword=' + encodeURIComponent(keyword) + '&limit=' + limit, { headers: { Referer: 'https://cp.chksz.top/' } })
  if (body?.code !== 200) throw new Error('搜索失败')
  const items = Array.isArray(body.data) ? body.data : (body.data?.songs || [])
  if (!items.length) throw new Error('未找到相关歌曲')
  return items.slice(0, limit).map(s => ({
    name: s.name || '', singer: s.artists || '',
    albumName: typeof s.album === 'string' ? s.album : (s.album?.name || ''),
    id: s.id, source: 'wy', interval: formatDuration(s.duration),
    meta: { picture: s.picUrl || '', wy: { id: s.id, url_id: s.id, lyric_id: s.id } },
  }))
}

async function searchKuwo(keyword, limit) {
  const results = []
  for (let page = 1; results.length < limit && page <= 3; page++) {
    try {
      const body = await httpGet('https://oiapi.net/api/Kuwo?msg=' + encodeURIComponent(keyword) + '&n=' + page)
      const d = body?.data
      if (!d?.song) break
      results.push({
        name: d.song || '', singer: d.singer || '', albumName: d.album || '',
        id: d.rid || ('kw_' + Date.now() + '_' + page), source: 'kw',
        interval: formatDuration(d.duration || d.time),
        meta: { kw: { id: d.rid || d.id } },
      })
    } catch (e) { break }
  }
  if (!results.length) throw new Error('未找到相关歌曲')
  return results
}

async function searchMigu(keyword, limit) {
  const results = []
  for (let page = 1; results.length < limit && page <= 3; page++) {
    try {
      const body = await httpGet('https://api.xcvts.cn/api/music/migu?gm=' + encodeURIComponent(keyword) + '&n=' + page + '&num=1&type=json')
      if (body?.code !== 200 || !body.title) break
      results.push({
        name: body.title || '', singer: body.artist || '', albumName: body.album || '',
        id: 'mg_' + Date.now() + '_' + page, source: 'mg', interval: body.duration || '00:00',
        meta: { mg: { id: body.id || '' } },
      })
    } catch (e) { break }
  }
  if (!results.length) throw new Error('未找到相关歌曲')
  return results
}

on(EVENT_NAMES.request, async ({ action, source, info }) => {
  if (action !== 'search') throw new Error('内置搜索源只支持 search')
  if (!info?.keyword) throw new Error('需要搜索关键词')
  const keyword = info.keyword.trim()
  const limit = Math.min(Number(info.limit) || 20, 30)
  switch (source) {
    case 'wy': return await searchChksz(keyword, limit)
    case 'kw': return await searchKuwo(keyword, limit)
    case 'mg': return await searchMigu(keyword, limit)
    default: throw new Error('该平台不支持搜索')
  }
})

send(EVENT_NAMES.inited, {
  sources: {
    wy: { name: '网易云音乐', actions: ['search'] },
    kw: { name: '酷我音乐', actions: ['search'] },
    mg: { name: '咪咕音乐', actions: ['search'] },
  },
})
`

const builtinSearchSourceName = "内置搜索源"
