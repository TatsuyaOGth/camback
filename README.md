# camback
Backup your photos. Bring your memories back.

[日本語](#日本語) | [English](#english)

## 日本語

camback は、カメラのメモリーカードの中身を SFTP で NAS に直接バックアップするコマンドラインツールです。macOS・Windows・Linux で同じように動きます。

- **新しいファイルだけをコピーします。** NAS にすでにある写真は飛ばすので、転送済みの写真をカードに残したままでもかまいません。
- **既存のファイルは決して上書きしません。** 同じ名前で中身の*違う*写真・動画が NAS にある場合は、何もコピーせずに中断します（`--on-conflict` で、そのファイルだけ飛ばすことも、隣に別の名前で保存することもできます）。カメラの管理ファイル（データベースや XML）のように内容が変わっていくファイルは、古いものを残したまま、更新日時（UTC）付きの名前で隣に保存します（例：`MEDIAPRO_20261016-002627Z.XML`）。
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

NAS 上のファイルとフォルダのパーミッションは、`[remote.<名前>]` の `file_mode` / `dir_mode`（例：`0o644` / `0o755`、`0o` を付けた 8 進数）で指定できます。指定しなければ NAS のサーバー任せです（Synology では `0o666` / `0o777` になることがあります）。指定した場合は、camback が新しく作ったファイルとフォルダだけを変え、すでにあるフォルダはそのままにします。サーバーが指定どおりに設定しない場合は、転送を始める前に中断します。

> **注意（Synology など ACL を使う NAS）：** Windows ACL が有効な共有フォルダでは、パーミッションの変更でファイルの ACL が変わることがあります。使う前に、1 枚だけ転送して File Station などでアクセス権が期待どおりか確認してください。

### 環境を点検する

設定を書いたとき、PC を変えたときは、`camback doctor` でまとめて点検できます。項目ごとに `OK` / `WARN`（警告）/ `FAIL`（問題）と、`fix:` に対処方法を表示します。

```sh
camback doctor               # NAS には何も書き込まない
camback doctor --write-test  # 各カメラの dest にプローブファイルを書いて消す
```

- exiftool が見つかるか、そのバージョン
- 設定ファイルの場所（`--config` / `CAMBACK_CONFIG` / 既定の場所のどれか）と、検証の結果
- remote ごとの接続：名前解決、TCP 接続、ホスト鍵、使われた認証方法（ssh-agent・鍵ファイル・パスワード）、SFTP
- カメラごとの `dest`：あるかどうかと、NAS 上の `sha256sum` が SFTP と同じファイルを読んでいるか（`shell_path_map` が正しいか）。`--write-test` を付けない場合は、`dest` にあるファイルを1つダウンロードして照合します
- `--write-test` を付けた場合だけ：`dest` に書き込めるか、`file_mode` / `dir_mode` が反映されるか（`dest` がまだなければ、その上にある既存のフォルダで確かめます）
- 挿さっているメモリーカード

`run` と同じく、初めて接続する NAS ではホスト鍵を表示し、確認してから `~/.ssh/known_hosts` に追加します。問題（`FAIL`）が1つでもあれば終了コードは `1`、警告だけなら `0` です。

### 使い方

```sh
camback              # カードを検出 → 計画を表示 → 確認 → 転送
camback --dry-run    # 計画を表示するだけ
camback --yes        # 確認せずに転送
camback --source <dir> --camera <id>   # DCIM フォルダや EXIF のない機器向け
camback --jobs 1     # 1ファイルずつ転送する（既定は4ファイルずつ同時に転送）
camback --on-conflict rename   # 衝突したファイルを隣に別の名前で保存する
```

同時に転送するファイルの数は、`--jobs`（1〜8）か設定ファイルの `[defaults] jobs` で変えられます。小さいファイルが多いカードほど速くなりますが、NAS の負荷（同時に動く `sha256sum` の数）も増えます。

同じ名前で中身の違うファイルが NAS にある（衝突する）ときの動作は、`--on-conflict` か設定ファイルの `[defaults] on_conflict` で選べます。

- `abort`（既定）：何も転送せずに中断します。
- `skip`：衝突したファイルだけ転送せず、ほかのファイルを転送します。飛ばしたファイルは最後に一覧を表示します。**カードにしか残っていないので、カードを消す前に確かめてください。**
- `rename`：NAS のファイルはそのまま残し、カードのファイルを更新日時（UTC）付きの名前で隣に保存します（例：`R0001185_20261016-002627Z.DNG`）。その名前も別のファイルで埋まっていれば中断します。

カメラがファイル番号を使い直したとき（カードのフォーマットや番号のリセットのあと）によく衝突します。この場合はどちらも別の写真なので、`rename` を使えば両方残せます。一度この名前で保存したファイルは、次からはどの設定でも保存済みとして飛ばします。

終了コード：`0` 成功（`skip` で飛ばしたファイルがあっても）、`1` エラー、`2` 衝突（転送の前に見つかった場合は何も転送していません。転送中に NAS 側にファイルが現れた場合は、それまでのファイルは転送済みです）、`3` キャンセル。

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

### 開発

```sh
go test ./...                    # 単体テスト（exiftool がなければ一部をスキップ）
go test -count=1 -tags e2e ./e2e/...   # Docker の OpenSSH サーバーを相手にした E2E テスト（Linux のみ、Docker と exiftool が必要）
```

### ライセンス

MIT

---

## English

camback copies everything on a camera's memory card straight to a NAS over
SFTP. It works the same on macOS, Windows and Linux.

- **Only new files are copied.** Photos already on the NAS are skipped, so you
  can leave old shots on the card.
- **Nothing is ever overwritten.** If a *different* photo or video with the
  same name is already on the NAS, camback stops before copying anything
  (or, with `--on-conflict`, skips it or saves it next to the other one).
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

Set `file_mode` / `dir_mode` under `[remote.<name>]` (in octal, e.g. `0o644`
/ `0o755`) to choose the permissions of what camback creates on the NAS.
Without them the server decides (Synology may use `0o666` / `0o777`). Only
the files and directories camback creates get them; existing directories
are left alone. If the server does not apply them, camback stops before
transferring anything.

> **Note (Synology and other NASes with ACLs):** on a shared folder with
> Windows ACLs, changing permissions can change a file's ACL. Transfer a
> single photo first and check its access rights, e.g. in File Station.

### Check your setup

After writing the config, or on a new PC, `camback doctor` checks
everything at once. Each item is `OK`, `WARN` or `FAIL`, with what to do
about it after `fix:`.

```sh
camback doctor               # writes nothing to the NAS
camback doctor --write-test  # writes and removes a probe file in each camera's dest
```

- exiftool, and its version
- which config file is used (from `--config`, `CAMBACK_CONFIG` or the
  default location) and whether it is valid
- the connection to each remote: name lookup, TCP connection, host key, how
  the login worked (ssh-agent, key file or password), SFTP
- each camera's `dest`: whether it exists, and whether `sha256sum` on the NAS
  reads the same file as SFTP (that `shell_path_map` is right). Without
  `--write-test` this downloads one file already in `dest` to compare.
- with `--write-test` only: whether camback can write in `dest`, and whether
  `file_mode` / `dir_mode` are applied (in the nearest existing folder above
  `dest` if `dest` does not exist yet)
- the memory cards that are inserted

As with `run`, an unknown NAS shows its host key and is added to
`~/.ssh/known_hosts` once you trust it. The exit code is `1` if anything
`FAIL`s, `0` if there are only warnings.

### Use

```sh
camback              # detect the card, show the plan, confirm, transfer
camback --dry-run    # show the plan only
camback --yes        # transfer without asking
camback --source <dir> --camera <id>   # for devices without a DCIM folder or EXIF
camback --jobs 1     # one file at a time (by default, 4 files at a time)
camback --on-conflict rename   # save conflicting files next to the other one
```

Set how many files are transferred at the same time with `--jobs` (1-8) or
`jobs` under `[defaults]` in the config. More is faster for cards with many
small files, but runs more `sha256sum` processes on the NAS at once.

Choose what happens when a different file with the same name is already on
the NAS (a conflict) with `--on-conflict` or `on_conflict` under
`[defaults]` in the config:

- `abort` (default): stop without transferring anything.
- `skip`: transfer everything else and list the conflicting files at the
  end. **They are only on the card, so check them before erasing it.**
- `rename`: keep the file on the NAS and save the one from the card next to
  it with its modification time in UTC, e.g. `R0001185_20261016-002627Z.DNG`.
  If that name is taken by yet another file, camback stops.

Conflicts usually mean the camera reused its file numbers (after formatting
the card or resetting the numbering): both are different photos, and
`rename` keeps both. A file once saved under its timestamped name is skipped
as already saved from then on, whatever `on_conflict` says.

Exit codes: `0` success (even if `skip` left files out), `1` error,
`2` conflict (nothing transferred when found before transferring; files
transferred until then when one appeared on the NAS during the transfer),
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

### Development

```sh
go test ./...                    # unit tests (some are skipped without exiftool)
go test -count=1 -tags e2e ./e2e/...   # end-to-end tests against OpenSSH in Docker (Linux only; needs Docker and exiftool)
```

### License

MIT
