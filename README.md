# netbird-helper

从 NetBird CDN 下载并处理 GeoLite2 地理定位数据库，用于 [NetBird](https://netbird.io)。

## 输出文件

| 文件 | 格式 | 说明 |
|------|------|------|
| `GeoLite2-City_<date>.mmdb` | MMDB | GeoLite2 City 二进制数据库，用于 IP 到位置的查询 |
| `geonames_<date>.db` | SQLite | 城市名称、行政区划、国家、时区等数据（来自 CSV 位置文件） |

文件名中包含日期戳（如 `GeoLite2-City_20240607.mmdb`），便于识别过期文件。

## 使用方式

```sh
go build -o netbird-helper .

# 下载并处理到 ./output 目录
./netbird-helper -o ./output

# 强制重新下载（即使文件已存在）
./netbird-helper -o ./output -force
```

### 参数

| 参数 | 说明 |
|------|------|
| `-o <dir>` | 输出目录（必填） |
| `-force` | 强制重新下载并覆盖已有文件 |

## 工作原理

1. 通过 CDN 返回的 `Content-Disposition` 头解析远程文件名，获取当前版本日期
2. 下载 GeoLite2-City tar.gz（MMDB）和 GeoLite2-City-CSV zip
3. 对每个下载文件校验 SHA-256 哈希
4. 解压并处理：
   - tar.gz 中的 `GeoLite2-City.mmdb` → 重命名为 `GeoLite2-City_<date>.mmdb`
   - zip 中的 `GeoLite2-City-Locations-en.csv` → 导入为 SQLite 数据库 `geonames_<date>.db`

## 自动发布

GitHub Actions 工作流（`.github/workflows/daily-release.yml`）每天 UTC 时间 00:00 和 12:00 自动运行：

1. 编译二进制文件
2. 下载并处理最新的数据库
3. 创建 GitHub Release，标签为 `geolite-<date>`（如 `geolite-20240607`）
4. 如果该日期的 Release 已存在则跳过——不会重复发布

也可通过 GitHub Actions 界面手动触发。
