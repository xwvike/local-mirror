#!/bin/bash

# 创建20000个空文件的脚本
# 使用方法: ./create_empty_files.sh [目标目录]

# 设置目标目录
TARGET_DIR=${1:-"../test"}

# 创建目标目录（如果不存在）
mkdir -p "$TARGET_DIR"

echo "开始创建20个空文件到目录: $TARGET_DIR"

# 使用循环创建文件
for i in {1..20}; do
    touch "$TARGET_DIR/file_$(printf "%05d" $i).txt"
done

echo "完成！已创建20个空文件在: $TARGET_DIR"