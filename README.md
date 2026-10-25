# camback
Backup your photos. Bring your memories back.

[日本語](#日本語) | [English](#english)

## 日本語

camback は、カメラのメモリーカードの中身を SFTP で NAS に直接バックアップするコマンドラインツールです。macOS・Windows・Linux で同じように動きます。

- **新しいファイルだけをコピーします。** NAS にすでにある写真は飛ばすので、転送済みの写真をカードに残したままでもかまいません。
- **既存のファイルは決して上書きしません。** 同じ名前で中身の*違う*写真・動画が NAS にある場合は、何もコピーせずに中断します。カメラの管理ファイル（データベースや XML）のように内容が変わっていくファイルは、古いものを残したまま、更新日時（UTC）付きの名前で隣に保存します（例：`MEDIAPRO_20261016-002627Z.XML`）。
- **カメラを EXIF から自動で判定します。** カード上のすべての写真が、設定ファイルに登録した同じ1台のものでなければ中断するので、別の機材の写真が混ざりません。
- **先に計画を示してからコピーします。** 何をするかを表示し、確認してから転送します。`--dry-run` を付けると計画の表示だけで終わります。
- **すべてのファイルを検証します**（SHA-256）。検証が済んでから最終的な名前に変えます。

NAS 上の構成はカードと同じです：`<dest>/<カード上のパス>`（例：`/photo/GR3x/DCIM/100RICOH/R0001185.DNG`）。

### 必要なもの

- `PATH` の通った [ExifTool](https://exiftool.org/)
  - macOS：`brew install exiftool`
  - Windows：Windows 用の実行ファイルをダウンロードし、`exiftool(-k).exe` を `exiftool.exe` に名前を変える
- NAS 側で SFTP が有効な SSH サーバー。Synology DSM の場合は「コントロールパネル → ファイルサービス → FTP → SFTP」で有効にし、ポート番号が設定ファイルと一致しているか確認してください。

### インストール

```sh
go install github.com/TatsuyaOGth/camback/cmd/camback@latest
```

`./scripts/build.sh [出力先]` で、全 OS 向けの実行ファイルをまとめて作ることもできます。

### 設定

[config.example.toml](config.example.toml) を `~/.config/camback/config.toml`（Windows は `%AppData%\camback\config.toml`）にコピーして編集します。複数の PC で設定を共有したい場合は、同期フォルダに置いて環境変数 `CAMBACK_CONFIG` でその場所を指定してください。

カメラの `make` / `model` に書く文字列は、カードを挿して次のコマンドで確認できます。

```sh
camback inspect /Volumes/YOUR_CARD
```

### 使い方

```sh
camback              # カードを検出 → 計画を表示 → 確認 → 転送
camback --dry-run    # 計画を表示するだけ
camback --yes        # 確認せずに転送
camback --source <dir> --camera <id>   # DCIM フォルダや EXIF のない機器向け
```

終了コード：`0` 成功、`1` エラー、`2` 衝突（何も転送していない）、`3` キャンセル。

Ctrl-C を1回押すと、処理の切れ目で安全に止まります（転送中の一時ファイルは削除します）。もう1回押すと即座に終了します。

ログ（JSON Lines 形式、実行1回につき1ファイル）の保存先：`~/Library/Logs/camback`（macOS）、`%LocalAppData%\camback\logs`（Windows）、`~/.local/state/camback`（Linux）。

### SSH のパスワード入力をなくす

camback は ssh-agent → `~/.ssh/id_ed25519` / `id_ecdsa` / `id_rsa` → パスワード（1回の実行につき1回だけ）の順で認証を試します。パスフレーズ付きの鍵は、NAS がその鍵を受け付ける場合にだけパスフレーズを聞きます（空のまま Enter で、その鍵を飛ばします）。鍵の設定方法：

```sh
ssh-keygen -t ed25519
ssh-copy-id -p 22 me@nas.local     # または ~/.ssh/id_ed25519.pub を NAS の ~/.ssh/authorized_keys に追記
```

Synology では、権限が厳しく設定されていないと鍵によるログインが無視されます。

```sh
chmod 755 ~ && chmod 700 ~/.ssh && chmod 600 ~/.ssh/authorized_keys
```

初めて接続するときは NAS のホスト鍵のフィンガープリントを表示し、確認してから `~/.ssh/known_hosts` に追加します。ホスト鍵が変わっていた場合は、必ず中断します。`known_hosts` に camback が読めない行（対応していない鍵の種類など）があれば、その行を無視して警告を表示します（`@revoked` の行だけは、読めなければ中断します）。

### ライセンス

MIT

---

## English

camback copies everything on a camera's memory card straight to a NAS over
SFTP. It works the same on macOS, Windows and Linux.

- **Only new files are copied.** Photos already on the NAS are skipped, so you
  can leave old shots on the card.
- **Nothing is ever overwritten.** If a *different* photo or video with the
  same name is already on the NAS, camback stops before copying anything.
  Other files that change over time (camera databases, XML) are saved next to
  the old copy with their modification time in UTC, e.g.
  `MEDIAPRO_20261016-002627Z.XML`.
- **The camera is detected from EXIF.** Every photo on the card must belong to
  the same configured camera, so shots from different bodies never get mixed.
- **Plan first, then copy.** camback shows what it will do and asks before
  transferring. `--dry-run` stops after the plan.
- **Every file is verified** (SHA-256) before it gets its final name.

The NAS layout mirrors the card: `<dest>/<path on the card>`, for example
`/photo/GR3x/DCIM/100RICOH/R0001185.DNG`.

### Requirements

- [ExifTool](https://exiftool.org/) on your `PATH`
  - macOS: `brew install exiftool`
  - Windows: download the Windows executable and rename `exiftool(-k).exe`
    to `exiftool.exe`
- An SSH server with the SFTP subsystem enabled on the NAS. On Synology DSM:
  *Control Panel → File Services → FTP → SFTP*. Check that its port matches
  the one in your config.

### Install

```sh
go install github.com/TatsuyaOGth/camback/cmd/camback@latest
```

Or build binaries for every OS with `./scripts/build.sh [output-dir]`.

### Configure

Copy [config.example.toml](config.example.toml) to
`~/.config/camback/config.toml` (Windows: `%AppData%\camback\config.toml`)
and edit it. To share one config between PCs, put it in a synced folder and
point `CAMBACK_CONFIG` at it.

To find the `make` / `model` strings of a camera, insert its card and run:

```sh
camback inspect /Volumes/YOUR_CARD
```

### Use

```sh
camback              # detect the card, show the plan, confirm, transfer
camback --dry-run    # show the plan only
camback --yes        # transfer without asking
camback --source <dir> --camera <id>   # for devices without a DCIM folder or EXIF
```

Exit codes: `0` success, `1` error, `2` conflict (nothing transferred),
`3` cancelled.

Press Ctrl-C once to stop safely at the next step (a partially written file is
removed); press it again to quit immediately.

Logs (JSON Lines, one file per run) are written to `~/Library/Logs/camback`
(macOS), `%LocalAppData%\camback\logs` (Windows) or `~/.local/state/camback`
(Linux).

### SSH without typing a password

camback tries ssh-agent, then `~/.ssh/id_ed25519` / `id_ecdsa` / `id_rsa`,
then asks for the password once per run. The passphrase of an encrypted key is
asked only when the NAS accepts that key (press Enter on an empty prompt to
skip it). To set up a key:

```sh
ssh-keygen -t ed25519
ssh-copy-id -p 22 me@nas.local     # or append ~/.ssh/id_ed25519.pub to ~/.ssh/authorized_keys on the NAS
```

On Synology, key login is ignored unless the permissions are strict:

```sh
chmod 755 ~ && chmod 700 ~/.ssh && chmod 600 ~/.ssh/authorized_keys
```

The first connection shows the NAS host key fingerprint and asks before
adding it to `~/.ssh/known_hosts`. A changed host key always aborts.
Lines of `known_hosts` that camback cannot parse (such as unsupported key
types) are ignored with a warning, except `@revoked` lines, which abort.

### License

MIT
