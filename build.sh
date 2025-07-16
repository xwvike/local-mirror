#!/bin/bash
# Go 交叉编译构建脚本 - Bash版本（适用于 Linux/macOS）
# 支持构建多个平台的二进制文件

APP_NAME="local-mirror"
MAIN_PATH="./cmd/local-mirror"
OUTPUT_DIR="dist"

# 创建输出目录
rm -rf "$OUTPUT_DIR"
mkdir -p "$OUTPUT_DIR"

# 定义目标平台 (GOOS/GOARCH)
platforms=(
    "windows/amd64"
    "windows/386" 
    "windows/arm64"
    "linux/amd64"
    "linux/386"
    "linux/arm64"
    "linux/arm"
    "darwin/amd64"
    "darwin/arm64"
)

echo "开始构建 $APP_NAME..."
echo "输出目录: $OUTPUT_DIR"

for platform in "${platforms[@]}"
do
    platform_split=(${platform//\// })
    GOOS=${platform_split[0]}
    GOARCH=${platform_split[1]}
    
    # 确定文件扩展名
    if [ $GOOS = "windows" ]; then
        extension='.exe'
    else
        extension=''
    fi
    
    output_name="$APP_NAME-$GOOS-$GOARCH$extension"
    output_path="$OUTPUT_DIR/$output_name"
    
    echo "构建 $GOOS/$GOARCH..."
    
    # 构建
    if CGO_ENABLED=0 GOOS=$GOOS GOARCH=$GOARCH go build -ldflags "-s -w" -o "$output_path" "$MAIN_PATH"; then
        file_size=$(du -h "$output_path" | cut -f1)
        echo "  ✓ $output_name ($file_size)"
    else
        echo "  ✗ $output_name 构建失败"
    fi
done

echo ""
echo "构建完成！"
echo "查看构建结果:"
ls -lh "$OUTPUT_DIR"
