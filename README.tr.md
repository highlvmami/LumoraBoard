# LumoraBoard

[English](README.md) · **Türkçe**

Gerçek zamanlı, ortak kullanılan bir beyaz tahta. Birden fazla kişi aynı tahtaya aynı anda çizer; herkes diğerlerinin çizimlerini ve imleçlerini anında görür. Arka uç Go ve WebSocket ile, her oda için ayrı bir goroutine olacak şekilde yazıldı; ön yüz SvelteKit ile yapıldı.

**Canlı demo:** https://lumoraboard-demo.onrender.com

Demo, Render'ın ücretsiz planında çalışıyor. Yaklaşık 15 dakika kimse girmezse uykuya geçer; sonraki ilk girişte uyanması bir dakikayı bulabilir.

## Özellikler

- **Canlı çizim:** kalem, dikdörtgen, elips, ok, yazı ve yapışkan not; renk ve kalınlık seçenekleriyle. Değişiklikler odadaki herkese anında ulaşır.
- **Canlı imleçler:** diğer kişilerin nereyi gösterdiğini isimleriyle birlikte görürsün.
- **Sahipli odalar:** odayı kim oluşturursa sahibi odur ve kimin gireceğine o karar verir.
- **Davetler:** oda sahibi düzenleme ya da sadece izleme daveti paylaşır; davet bir link ya da `K7QM-2XRA` gibi kısa bir kod olabilir. İzleyiciler her şeyi canlı görür ama değişiklik yapamaz.
- **Oda sohbeti:** her odanın kendi sohbeti var. Bir mesaj tahtadaki bir nesneyi gösterebilir.
- **Dışa ve içe aktarma:** tarayıcıda PNG ve SVG; sunucuda yüksek çözünürlüklü PNG, PDF ve JSON yedek; JSON yedeği yeni bir tahta olarak içe aktarma.
- **Yeniden başlatmaya dayanıklı:** tahtalar Postgres'te saklanır; bağlantısı kopup geri gelen biri kaçırdıklarını yakalar.
- **Yatay ölçeklenme:** birden fazla sunucu yükü paylaşabilir; her tahta aynı anda tek bir sunucuda yaşar.

## Nasıl kullanılır

1. Siteyi aç ve adını yaz.
2. Kendi tahtanı açmak için **Create a room**, davet edildiğin bir odaya girmek için **Join a room** seç.
3. Oda sahibiysen **Invite** menüsünden *can edit* (düzenleyebilir) ya da *view only* (sadece izler) linkini kopyala. Yanındaki kod da aynı işi görür.
4. Davet ettiğin kişi linki açar ya da **Join a room** deyip linki yapıştırır veya kodu yazar. Davetin verdiği rolle odaya girer.

**← Menu** menüye döndürür, **Sign out** yeni bir isimle baştan başlatır.

## Nasıl çalışıyor

Projenin amacı eşzamanlılık (concurrency), bu yüzden tasarım paylaşılan durumu en aza indiriyor.

- **Her oda bir goroutine (aktör modeli).** Her oda kendi tahta durumuna sahip ve ona dokunan tek kod o. Bağlantılar odaya kanallar üzerinden mesaj gönderir; tahtanın etrafında kilit yok.
- **Hub.** Oda tablosunu tek bir goroutine yönetir. Odaya katılma, oda açma ve boşta kalan odayı kapatma onun üzerinden geçer; böylece odanın yaşam döngüsünde yarış durumu olmaz.
- **Çakışma çözümü.** Kabul edilen her değişiklik odadan bir sıra numarası alır. Aynı nesne için son değişiklik kazanır (LWW) ve her istemci değişiklikleri odanın sırasıyla uygular; bu yüzden bütün ekranlar aynı sonuca varır.
- **Geri basınç.** Her bağlantının sınırlı bir giden kuyruğu var. Yetişemeyen bir istemci odayı yavaşlatmak yerine bağlantıdan düşürülür. İmleç güncellemeleri bilerek kayıplıdır ve ilk onlar atılır.
- **Arkadan yazma (write-behind).** Kabul edilen değişiklikler toplu halde, oda goroutine'inin dışında Postgres'e yazılır. Her 1000 değişiklikte bir alınan anlık görüntü yüklemeyi hızlı tutar. Veritabanı geride kalırsa oda, veritabanı yetişene kadar yeni değişiklik almayı durdurur; kabul edilmiş değişiklikler asla kaybolmaz.
- **Yeniden bağlanma.** Geri bağlanan istemci gördüğü son sıra numarasını söyler. Oda sadece kaçırılanları gönderir; çok gerideyse tam bir anlık görüntü yollar.
- **Birden fazla sunucu.** Her tahta, Postgres'teki bir kira (lease) satırı ve bir dönem (epoch) numarasıyla tek bir sunucuya aittir. Başka bir sunucuya gelen istemciler imzalı bir WebSocket üzerinden sahibe yönlendirilir. Sahip çökerse başka bir sunucu birkaç saniye içinde tahtayı devralır; epoch sayesinde geç uyanan eski sahip yenisinin üzerine yazamaz.
- **Dışa aktarma işleri.** Sunucu tarafı dışa aktarmalar, sınırlı bir kuyruğun arkasındaki sabit sayıda işçi tarafından yapılır. Kuyruk doluysa iş yığılmaz, 503 döner.

## Kullanılan teknolojiler

| Bölüm | Teknoloji |
|---|---|
| Arka uç | Go 1.25, `coder/websocket`, `pgx` |
| Ön yüz | SvelteKit (Svelte 5), TypeScript, Canvas |
| Veritabanı | PostgreSQL |
| Yayına alma | Docker (tek imaj), Render, Neon |
| Testler | `go test -race`, Vitest, svelte-check, golangci-lint |

## Klasör yapısı

| Yol | İçerik |
|---|---|
| `backend/cmd/server` | Giriş noktası ve ayarlar |
| `backend/internal/room` | Hub, oda aktörleri, kalıcılık |
| `backend/internal/ws` | WebSocket işleyicisi, sunucular arası yönlendirme |
| `backend/internal/auth` | Giriş, oturumlar, roller, davetler |
| `backend/internal/cluster` | Çoklu sunucu için oda kiraları |
| `backend/internal/export` | Dışa ve içe aktarma işleri |
| `backend/internal/store` | Postgres ve bellek içi depolama |
| `frontend/` | SvelteKit uygulaması |
| `docs/roadmap.md` | Geliştirme yol haritası |

## Yerelde çalıştırma

Gerekenler: Go 1.25+, Node 22+, Docker.

```sh
cd frontend && npm install && cd ..
make dev        # Postgres + :8080'de arka uç + :5173'te ön yüz
make test       # go test -race, vitest, svelte-check
make lint       # go vet + golangci-lint
```

Sonra http://localhost:5173 adresini aç. `make dev` sadece isimle girişi açar.

## Ayarlar

| Değişken | Anlamı |
|---|---|
| `LUMORA_DATABASE_URL` | Postgres bağlantı adresi. Verilmezse tahtalar sunucu yeniden başlayana kadar bellekte tutulur. |
| `LUMORA_PUBLIC_URL` | Tarayıcıların uygulamaya ulaştığı adres. Varsayılan `http://localhost:5173`; Render'da `RENDER_EXTERNAL_URL` kullanılır. |
| `LUMORA_DEV_LOGIN=1` | Sadece isimle giriş. |
| `LUMORA_GITHUB_CLIENT_ID`, `LUMORA_GITHUB_CLIENT_SECRET` | GitHub ile girişi açar (PKCE'li OAuth). |
| `LUMORA_GOOGLE_CLIENT_ID`, `LUMORA_GOOGLE_CLIENT_SECRET` | Google ile girişi açar. |
| `LUMORA_GUESTS=view` | Daveti olmayanların tahtaları sadece izleyebilmesini sağlar. |
| `LUMORA_ALLOWED_ORIGINS` | WebSocket açmasına izin verilen adresler, virgülle ayrılmış. Varsayılan: localhost ve genel adresin kendisi. |
| `LUMORA_CLUSTER_SECRET` | Çoklu sunucu modunu açar (en az 16 karakter, her sunucuda aynı). |
| `LUMORA_ADVERTISE_URL`, `LUMORA_INSTANCE` | Diğer sunucuların bu sunucuya nasıl ulaşacağı ve sunucunun adı. |
| `PORT` / `LUMORA_ADDR` | Dinlenecek adres. Varsayılan `:8080`. |

Hiçbir giriş yöntemi ayarlanmazsa sunucu açık modda çalışır: herkes her tahtaya çizebilir.

## Yayına alma (Render + Neon)

`Dockerfile`, Go sunucusunu ve derlenmiş ön yüzü içeren tek bir imaj üretir; ikisi aynı adresten sunulur.

1. [Neon](https://neon.tech) üzerinde ücretsiz bir Postgres veritabanı oluştur ve bağlantı adresini kopyala.
2. [Render panelinde](https://dashboard.render.com) New → Blueprint seç ve bu repoyu bağla. Servisi `render.yaml` kurar.
3. `LUMORA_DATABASE_URL` değişkenine Neon adresini yaz. Render imajı derler ve `main` dalına yapılan her push'ta yeniden yayına alır.

`GET /healthz` sunucunun sağlığını, `GET /version` yayındaki commit'i gösterir.
