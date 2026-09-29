# LumoraBoard Yol Haritası

Gerçek zamanlı ortak beyaz tahta. Backend Go + WebSocket, frontend Svelte (SvelteKit, Svelte 5).
Asıl amaç: **eşzamanlılık becerisini ölçülebilir şekilde göstermek.** Bu yüzden her fazın bir "bitti sayılır" ölçütü var ve son fazlar sayılarla (yük testi, gecikme, sızıntı testi) kanıt üretmeye ayrıldı.

Varsayım: sıfırdan başlıyoruz, tek geliştirici, monorepo.

---

## Karar: çakışma çözümü LWW (29.09.2026)

| | **LWW (son yazan kazanır), sunucu sıralı** | **CRDT (ör. Yjs / Automerge ya da kendi yazacağın)** |
|---|---|---|
| Nasıl | Her nesne ayrı; sunucu her işleme artan `seq` verir, aynı alana gelen iki güncellemeden sonraki kazanır | İşlemler her sırada uygulansa da aynı sonuca yakınsar |
| Zorluk | Düşük-orta | Yüksek (Go tarafında olgun kütüphane az) |
| Çevrimdışı düzenleme | Zayıf | Güçlü |
| Eşzamanlılık vitrini | Go tarafında goroutine/kanal/sıralama işi öne çıkar | Algoritma öne çıkar, Go tarafı daha çok "röle" olur |

**Seçilen:** v1'de nesne başına LWW + sunucu otoritesi. Beyaz tahtada çakışmalar genelde aynı nesnenin aynı alanına denk gelir ve LWW burada yeterli. Protokolü, ileride CRDT'ye geçişe izin verecek şekilde (op tabanlı, versiyonlu zarf) tasarlıyoruz. CRDT'yi Faz 8'de opsiyonel iş olarak bıraktım.

---

## Mimari özet

```
Tarayıcı (Svelte)                      Go sunucusu
┌──────────────┐   WebSocket   ┌─────────────────────────────────────┐
│ Canvas + araç│◄────────────►│ Conn: readPump / writePump goroutine │
│ yerel store  │              │        │ (sınırlı send kanalı)       │
│ bekleyen ops │              │        ▼                              │
└──────────────┘              │ Room aktörü (oda başına 1 goroutine)  │
                              │  - tek sahip: durum + seq             │
                              │  - fan-out, presence                  │
                              │        │                              │
                              │        ▼                              │
                              │ Persister (toplu yazma goroutine'i)   │──► Postgres
                              │ Bus (çok instance için)               │──► Redis/NATS
                              └─────────────────────────────────────┘
```

Temel ilke: **oda durumu tek bir goroutine'e aittir** (actor modeli). Durum üzerinde mutex yok; her şey kanallardan geçer. Bu hem yarış koşullarını ortadan kaldırır hem de mülakatta anlatması kolay, net bir tasarımdır.

---

## Faz 0: Kurulum

- Monorepo: `backend/` (Go 1.23+), `frontend/` (SvelteKit + TypeScript), `docs/`.
- `docker-compose` ile Postgres (ve sonra Redis).
- `Makefile`: `make dev`, `make test`, `make lint`.
- CI (GitHub Actions): `go test -race ./...`, `golangci-lint`, `go vet`, frontend için `vitest` + `svelte-check`.
- WebSocket kütüphanesi: `github.com/coder/websocket` (context dostu) ya da `gorilla/websocket`. Önerim `coder/websocket`.

**Bitti sayılır:** boş sunucu ve boş Svelte sayfası tek komutla ayağa kalkıyor, CI yeşil.

## Faz 1: Eşzamanlılık çekirdeği (tek oda, tek sunucu)

- **Hub:** oda adı → Room eşlemesi. Odalar talep edildiğinde açılır, boşalınca bir süre sonra kapanır.
- **Room aktörü:** `join`, `leave`, `incoming op` kanallarını `select` ile dinleyen tek goroutine.
- **Client:** her bağlantı için `readPump` ve `writePump`. Yazma tarafı sınırlı (buffered) kanal kullanır.
- **Yavaş tüketici politikası:** send kanalı dolarsa istemci düşürülür (oda bloklanmaz). Bu, gösterilecek en önemli kararlardan biri.
- Ping/pong ile ölü bağlantı tespiti, okuma/yazma zaman aşımları, mesaj boyutu limiti.
- `context` ile iptal zinciri; `errgroup` ile düzgün kapanış (SIGTERM'de tüm odalar temiz kapanır).
- Mesaj zarfı (JSON, versiyonlu): `{ v, type, room, seq, clientOpId, payload }`.

**Bitti sayılır:** iki tarayıcı sekmesi aynı odada ham mesajları birbirine yansıtıyor; `-race` temiz; `goleak` testleri goroutine sızıntısı olmadığını gösteriyor.

## Faz 2: Çizim modeli ve senkronizasyon

- Nesne tipleri: serbest çizgi (stroke), dikdörtgen, elips, ok, metin, yapışkan not.
- İşlemler: `add`, `update` (kısmi alanlar), `delete`, `move`, `reorder`.
- Sunucu her işleme odada artan `seq` atar; istemciye `ack(clientOpId, seq)` döner.
- **Yeniden bağlanma:** istemci son gördüğü `seq` ile gelir; sunucu aradaki işlemleri gönderir, çok gerideyse tam snapshot yollar.
- Çakışma çözümü: yukarıdaki karara göre (varsayılan LWW).
- Çizgi akışı: kalem hareketi sırasında noktalar parça parça (örn. 50 ms'de bir) gönderilir, bitince tek nesneye kesinleşir.

**Bitti sayılır:** iki istemci aynı nesneyi aynı anda sürüklediğinde ikisi de aynı son duruma yakınsıyor; ağ kesip bağlayınca kayıp yok.

## Faz 3: Svelte frontend

- Canvas2D ile çizim (önce saf Canvas, gerekirse sonra katmanlı canvas). Kaydırma ve yakınlaştırma.
- Araçlar: kalem, şekil, metin, seçim, silgi.
- Svelte 5 runes ile store: `confirmed` durum + `pending` (iyimser) işlemler. Ack gelince pending'den düşer, çakışmada yeniden uygulanır.
- **Presence:** diğer kullanıcıların imleçleri ve isimleri. İmleç mesajları ~20–30 Hz'e kısılır (throttle) ve kalıcı depoya yazılmaz.
- Bağlantı durumu göstergesi, otomatik yeniden bağlanma (üstel geri çekilme + jitter).

**Bitti sayılır:** iki kişi aynı tahtada rahatça çizim yapıp birbirinin imlecini görüyor.

## Faz 4: Kalıcılık

- Postgres: `boards`, `board_snapshots`, `board_ops` tabloları.
- **Persister goroutine'i:** oda aktöründen kanal üzerinden işlemleri alır, belirli aralıkla ya da belirli sayıda birikince toplu yazar (write-behind). Aktör veritabanını asla beklemez.
- Geri basınç: persister kuyruğu dolarsa ne olacağı açıkça tanımlanır (ör. oda yeni işlem kabulünü yavaşlatır, metrik alarmı).
- Periyodik snapshot + eski op'ların sıkıştırılması.
- Oda ilk açılışta son snapshot + sonraki op'lardan ayağa kalkar.

**Bitti sayılır:** sunucu yeniden başlatılınca tahta aynen geri geliyor; yük altında veritabanı yavaşlasa bile canlı işbirliği gecikmiyor.

## Faz 5: Kullanıcı özellikleri (OAuth, oda sohbeti, dışa aktarma)

### OAuth ile giriş
- GitHub ve Google sağlayıcıları, `golang.org/x/oauth2` ile Authorization Code + PKCE akışı.
- Oturum: `HttpOnly`, `Secure`, `SameSite=Lax` çerez; sunucu tarafında oturum tablosu (Postgres).
- WebSocket el sıkışmasında çerez doğrulanır ve `Origin` kontrol edilir; kimliği doğrulanmamış bağlantı odaya giremez.
- Tablolar: `users`, `oauth_accounts` (bir kullanıcıya birden fazla sağlayıcı bağlanabilir), `sessions`.
- Yetki: tahta başına `owner / editor / viewer` rolleri, davet ya da paylaşım linki ile katılım. Viewer'dan gelen çizim işlemleri Room aktöründe reddedilir.
- İsteğe bağlı misafir modu: giriş yapmadan salt okunur izleme.

### Oda sohbeti
- Aynı WebSocket bağlantısı üzerinden yeni mesaj tipleri: `chat.send`, `chat.message`, `chat.typing`.
- Sohbet mesajları Room aktöründen geçer (tahta ile aynı sıralama garantisi), persister ile Postgres'e toplu yazılır.
- Geçmiş: odaya girişte son N mesaj, yukarı kaydırınca sayfalı yükleme (REST).
- "Yazıyor..." göstergesi geçicidir, kalıcı depoya yazılmaz ve kısılır.
- Kullanıcı başına hız sınırı (token bucket) ve mesaj uzunluğu limiti; spam, odayı yavaşlatamaz.
- Güzel ek: sohbetteki bir mesajı tahtadaki bir nesneye bağlamak (tıklayınca oraya kaydırır).

### Tahtayı indirme / dışa aktarma
- **İstemci tarafı:** PNG ve SVG, doğrudan canvas'tan. Hızlı ve sunucuya yük getirmez.
- **Sunucu tarafı:** PDF, yüksek çözünürlüklü PNG ve tahtanın JSON yedeği (tekrar içe aktarılabilir).
- Sunucu tarafı dışa aktarma bir **iş kuyruğu + sınırlı worker havuzu** ile çalışır: iş kanala girer, N worker işler, ilerleme WebSocket ile istemciye akar, bitince indirme linki verilir. İptal `context` ile yapılır. Aynı anda gelen 100 dışa aktarma isteği sunucuyu çökertmez, sıraya girer. Bu, eşzamanlılık vitrinine doğrudan katkı sağlar.
- JSON içe aktarma: yedekten yeni tahta oluşturma.

**Bitti sayılır:** GitHub/Google ile giriş yapılıyor, roller uygulanıyor; odada sohbet edilebiliyor ve geçmiş kalıcı; tahta PNG, SVG, PDF ve JSON olarak indirilebiliyor.

## Faz 6: Yatay ölçekleme

- Birden fazla Go instance'ı. İki seçenek:
  - **Oda sahipliği:** her oda tek instance'ta yaşar (tutarlı hash + yük dengeleyicide yönlendirme). Basit ve tutarlı.
  - **Pub/sub fan-out:** Redis Pub/Sub ya da NATS ile instance'lar arası yayın; sıralama için oda başına tek "lider".
- Önerim: oda sahipliği + NATS/Redis yalnızca presence ve instance'lar arası yönlendirme için.
- Instance düşerse odanın başka instance'ta snapshot'tan ayağa kalkması.
- Oturumlar Postgres'te olduğu için hangi instance'a bağlanılırsa bağlanılsın giriş geçerli kalır; dışa aktarma kuyruğu da paylaşımlı hale gelir (Redis/NATS JetStream).

**Bitti sayılır:** iki instance'a bağlı istemciler aynı odada sorunsuz çalışıyor; bir instance öldürülünce istemciler yeniden bağlanıp devam ediyor.

## Faz 7: Eşzamanlılık vitrini (kanıt üretme)

Bu faz projenin "neden etkileyici" olduğunu gösteren kısım.

- **Yük testi aracı** (Go ile yazılmış): binlerce sanal istemci, gerçekçi çizim, imleç ve sohbet trafiği. Alternatif olarak k6.
- Ölçümler: eşzamanlı bağlantı sayısı, fan-out gecikmesi p50/p99, düşürülen yavaş istemci sayısı, dışa aktarma kuyruk bekleme süresi, bellek/goroutine sayısı.
- Prometheus metrikleri + Grafana panosu; `pprof` ile CPU/bellek profili.
- Testler: `-race`, `goleak`, op uygulama mantığı için **fuzz test**, rastgele sıralı işlemlerle yakınsama testi (property-based).
- Benchmark'lar: JSON vs. binary (MessagePack/protobuf) zarfı, fan-out'ta tampon yeniden kullanımı (`sync.Pool`).
- README: mimari diyagramı, tasarım kararları ve gerekçeleri, yük testi sonuçları (grafiklerle).

**Bitti sayılır:** README'de "tek instance'ta N eşzamanlı istemci, p99 fan-out X ms" gibi somut, tekrarlanabilir sayılar var.

## Faz 8: Ek özellik fikirleri (opsiyonel)

İşbirliği:
- **Takip modu:** bir kullanıcının ekranını (viewport) canlı takip etme, sunum yaparken çok işe yarar.
- **Nesne yorumları:** tahtadaki bir nesneye iğne bırakıp altında yorum dizisi açma.
- **Emoji tepkileri ve lazer işaretçi:** geçici, kalıcı olmayan efektler (presence kanalından gider).
- **Oylama ve zamanlayıcı:** retro/beyin fırtınası oturumları için yapışkan notlara oy verme, ortak geri sayım.

Tahta:
- **Sürüm geçmişi / zamanda yolculuk:** op log'u tekrar oynatarak tahtanın herhangi bir andaki halini gösterme, eski sürüme dönme. Mevcut op log mimarisinin doğal bir ürünü.
- Kullanıcı başına geri al / yinele (yalnızca kendi işlemleri).
- Görsel yükleme (S3 uyumlu depo), şablonlar (akış şeması, kanban, retro), mini harita, nesne kilitleme, katmanlar.

Platform:
- Tahta listesi / panosu, favoriler, son açılanlar.
- Bildirimler: davet edildiğinde ya da bahsedildiğinde (@mention) e-posta.
- Mobil ve dokunmatik destek, karanlık tema.
- CRDT'ye geçiş denemesi (Faz 2'deki protokol buna hazır).
- Canlı demo için dağıtım (Fly.io ya da benzeri).

---

## Önerilen sıra ve kilometre taşları

1. **M1, "Canlı yankı":** Faz 0 + Faz 1
2. **M2, "Birlikte çizebiliyoruz":** Faz 2 + Faz 3
3. **M3, "Kaybolmuyor":** Faz 4
4. **M4, "Gerçek bir ürün":** Faz 5 (OAuth, sohbet, dışa aktarma)
5. **M5, "Ölçekleniyor ve kanıtlı":** Faz 6 + Faz 7
6. Faz 8 zaman kaldıkça, en çok ilgini çekenlerden başlayarak.

M2 bittiğinde proje zaten gösterilebilir bir demo olur. M4 onu gerçek kullanıcıların kullanabileceği bir ürüne çevirir. M5, eşzamanlılık iddiasını sayılarla destekleyen kısımdır.
