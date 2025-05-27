package main

import (
	"encoding/json"
	"fmt"
)

// Node 表示树节点结构
type Node struct {
	Name     string                   `json:"name"`
	Path     string                   `json:"path"`
	Type     string                   `json:"type"`
	Children []map[string]interface{} `json:"children"`
}

// DiffResult 表示差异结果
type DiffResult struct {
	Path   string `json:"path"`
	Type   string `json:"type"`
	Action string `json:"action"` // "add", "reget"
	Name   string `json:"name"`
}

// findDifferences 比较两个JSON树结构，以a为基准
func findDifferences(a, b map[string]interface{}) []DiffResult {
	var diffs []DiffResult

	// 递归比较函数
	var compare func(nodeA, nodeB map[string]interface{}, parentPath string)
	compare = func(nodeA, nodeB map[string]interface{}, parentPath string) {
		// 获取当前节点信息
		nameA, _ := nodeA["name"].(string)
		pathA, _ := nodeA["path"].(string)
		typeA, _ := nodeA["type"].(string)

		currentPath := pathA
		if parentPath != "" {
			currentPath = parentPath + "/" + nameA
		}

		// 如果b中没有对应节点，标记为add
		if nodeB == nil {
			diffs = append(diffs, DiffResult{
				Path:   currentPath,
				Type:   typeA,
				Action: "add",
				Name:   nameA,
			})
			// 递归处理所有子节点
			childrenA, okA := nodeA["children"].([]interface{})
			if okA {
				for _, childA := range childrenA {
					if childAMap, ok := childA.(map[string]interface{}); ok {
						compare(childAMap, nil, currentPath)
					}
				}
			}
			return
		}

		// 比较基本属性
		nameB, _ := nodeB["name"].(string)
		typeB, _ := nodeB["type"].(string)

		if nameA != nameB || typeA != typeB {
			diffs = append(diffs, DiffResult{
				Path:   currentPath,
				Type:   typeA,
				Action: "reget",
				Name:   nameA,
			})
		}

		// 比较children
		childrenA, okA := nodeA["children"].([]interface{})
		childrenB, okB := nodeB["children"].([]interface{})

		if !okA {
			childrenA = []interface{}{}
		}
		if !okB {
			childrenB = []interface{}{}
		}

		// 将childrenB转换为map以便快速查找
		childrenBMap := make(map[string]map[string]interface{})
		for _, childB := range childrenB {
			if childBMap, ok := childB.(map[string]interface{}); ok {
				if name, ok := childBMap["name"].(string); ok {
					childrenBMap[name] = childBMap
				}
			}
		}

		// 遍历A的children
		for _, childA := range childrenA {
			if childAMap, ok := childA.(map[string]interface{}); ok {
				if nameA, ok := childAMap["name"].(string); ok {
					// 在B中查找对应的child
					if childBMap, exists := childrenBMap[nameA]; exists {
						// 递归比较
						compare(childAMap, childBMap, currentPath)
						// 从map中删除已处理的项
						delete(childrenBMap, nameA)
					} else {
						// B中不存在，标记为deleted
						compare(childAMap, nil, currentPath)
					}
				}
			}
		}
	}

	// 开始比较
	compare(a, b, "")

	return diffs
}

// findDifferencesFromJSON 从JSON字符串比较差异
func findDifferencesFromJSON(jsonA, jsonB string) ([]DiffResult, error) {
	var a, b map[string]interface{}

	if err := json.Unmarshal([]byte(jsonA), &a); err != nil {
		return nil, fmt.Errorf("解析JSON A失败: %v", err)
	}

	if err := json.Unmarshal([]byte(jsonB), &b); err != nil {
		return nil, fmt.Errorf("解析JSON B失败: %v", err)
	}

	return findDifferences(a, b), nil
}

func main() {
	// 示例数据
	jsonA := `{
    "name": "local-mirror",
    "path": ".",
    "type": "dir",
    "children": [
        {
            "name": "client.log",
            "path": "./client.log",
            "type": "file",
            "children": [],
            "metadata": {
                "modTime": "2025-05-27T11:10:48.428573769+08:00",
                "size": 0
            }
        },
        {
            "name": "cmd",
            "path": "./cmd",
            "type": "dir",
            "children": [
                {
                    "name": "main",
                    "path": "./cmd/main",
                    "type": "dir",
                    "children": [
                        {
                            "name": "main.go",
                            "path": "./cmd/main/main.go",
                            "type": "file",
                            "children": [],
                            "metadata": {
                                "modTime": "2025-05-26T14:05:38.437528304+08:00",
                                "size": 781
                            }
                        }
                    ],
                    "metadata": {
                        "modTime": "2025-04-18T16:13:21.356090825+08:00",
                        "size": 96
                    }
                }
            ],
            "metadata": {
                "modTime": "2025-04-18T16:13:21.356022033+08:00",
                "size": 96
            }
        },
        {
            "name": "config",
            "path": "./config",
            "type": "dir",
            "children": [
                {
                    "name": "config.go",
                    "path": "./config/config.go",
                    "type": "file",
                    "children": [],
                    "metadata": {
                        "modTime": "2025-05-23T15:21:16.517095383+08:00",
                        "size": 636
                    }
                }
            ],
            "metadata": {
                "modTime": "2025-05-19T13:53:11.116543706+08:00",
                "size": 96
            }
        },
        {
            "name": "dist",
            "path": "./dist",
            "type": "dir",
            "children": [
                {
                    "name": "local-mirror",
                    "path": "./dist/local-mirror",
                    "type": "file",
                    "children": [],
                    "metadata": {
                        "modTime": "2025-05-27T11:35:15.442507497+08:00",
                        "size": 4846771
                    }
                }
            ],
            "metadata": {
                "modTime": "2025-05-27T11:35:15.442761375+08:00",
                "size": 96
            }
        },
        {
            "name": "ds.mp4",
            "path": "./ds.mp4",
            "type": "file",
            "children": [],
            "metadata": {
                "modTime": "2025-05-23T15:17:32.79211685+08:00",
                "size": 11464963
            }
        },
        {
            "name": "go.mod",
            "path": "./go.mod",
            "type": "file",
            "children": [],
            "metadata": {
                "modTime": "2025-05-21T11:30:49.059528678+08:00",
                "size": 269
            }
        },
        {
            "name": "go.sum",
            "path": "./go.sum",
            "type": "file",
            "children": [],
            "metadata": {
                "modTime": "2025-05-21T11:30:49.060154177+08:00",
                "size": 1612
            }
        },
        {
            "name": "internal",
            "path": "./internal",
            "type": "dir",
            "children": [
                {
                    "name": "app",
                    "path": "./internal/app",
                    "type": "dir",
                    "children": [
                        {
                            "name": "app.go",
                            "path": "./internal/app/app.go",
                            "type": "file",
                            "children": [],
                            "metadata": {
                                "modTime": "2025-05-27T11:10:42.539091234+08:00",
                                "size": 2581
                            }
                        },
                        {
                            "name": "buildFileTree.go",
                            "path": "./internal/app/buildFileTree.go",
                            "type": "file",
                            "children": [],
                            "metadata": {
                                "modTime": "2025-05-27T10:54:57.206095438+08:00",
                                "size": 1358
                            }
                        },
                        {
                            "name": "client.go",
                            "path": "./internal/app/client.go",
                            "type": "file",
                            "children": [],
                            "metadata": {
                                "modTime": "2025-05-27T11:34:52.434808583+08:00",
                                "size": 9008
                            }
                        },
                        {
                            "name": "discovery.go",
                            "path": "./internal/app/discovery.go",
                            "type": "file",
                            "children": [],
                            "metadata": {
                                "modTime": "2025-05-26T10:20:28.87566378+08:00",
                                "size": 12
                            }
                        },
                        {
                            "name": "eventFilter.go",
                            "path": "./internal/app/eventFilter.go",
                            "type": "file",
                            "children": [],
                            "metadata": {
                                "modTime": "2025-05-27T10:39:34.268926124+08:00",
                                "size": 2096
                            }
                        },
                        {
                            "name": "fileWatch.go",
                            "path": "./internal/app/fileWatch.go",
                            "type": "file",
                            "children": [],
                            "metadata": {
                                "modTime": "2025-05-27T09:58:09.452119144+08:00",
                                "size": 577
                            }
                        },
                        {
                            "name": "initLogger.go",
                            "path": "./internal/app/initLogger.go",
                            "type": "file",
                            "children": [],
                            "metadata": {
                                "modTime": "2025-05-21T16:00:41.239040579+08:00",
                                "size": 1313
                            }
                        },
                        {
                            "name": "protocol.go",
                            "path": "./internal/app/protocol.go",
                            "type": "file",
                            "children": [],
                            "metadata": {
                                "modTime": "2025-05-26T15:13:59.717355024+08:00",
                                "size": 12594
                            }
                        },
                        {
                            "name": "server.go",
                            "path": "./internal/app/server.go",
                            "type": "file",
                            "children": [],
                            "metadata": {
                                "modTime": "2025-05-26T17:58:56.836327245+08:00",
                                "size": 8821
                            }
                        }
                    ],
                    "metadata": {
                        "modTime": "2025-05-26T15:56:46.627810331+08:00",
                        "size": 352
                    }
                }
            ],
            "metadata": {
                "modTime": "2025-04-18T16:13:21.3564442+08:00",
                "size": 96
            }
        },
        {
            "name": "pkg",
            "path": "./pkg",
            "type": "dir",
            "children": [
                {
                    "name": "jsonutil",
                    "path": "./pkg/jsonutil",
                    "type": "dir",
                    "children": [
                        {
                            "name": "diffJson.go",
                            "path": "./pkg/jsonutil/diffJson.go",
                            "type": "file",
                            "children": [],
                            "metadata": {
                                "modTime": "2025-05-27T11:34:52.434791542+08:00",
                                "size": 2821
                            }
                        }
                    ],
                    "metadata": {
                        "modTime": "2025-05-27T11:23:43.163290446+08:00",
                        "size": 96
                    }
                },
                {
                    "name": "utils",
                    "path": "./pkg/utils",
                    "type": "dir",
                    "children": [
                        {
                            "name": "helper.go",
                            "path": "./pkg/utils/helper.go",
                            "type": "file",
                            "children": [],
                            "metadata": {
                                "modTime": "2025-05-26T10:20:40.017271115+08:00",
                                "size": 2104
                            }
                        }
                    ],
                    "metadata": {
                        "modTime": "2025-04-18T16:13:21.357137534+08:00",
                        "size": 96
                    }
                }
            ],
            "metadata": {
                "modTime": "2025-05-27T11:23:34.114755411+08:00",
                "size": 128
            }
        },
        {
            "name": "server.log",
            "path": "./server.log",
            "type": "file",
            "children": [],
            "metadata": {
                "modTime": "2025-05-27T11:35:16.324267095+08:00",
                "size": 106
            }
        },
        {
            "name": "test.json",
            "path": "./test.json",
            "type": "file",
            "children": [],
            "metadata": {
                "modTime": "2025-05-27T11:10:17.020322487+08:00",
                "size": 11953
            }
        },
        {
            "name": "tools",
            "path": "./tools",
            "type": "dir",
            "children": [
                {
                    "name": "autoTcpTest.go",
                    "path": "./tools/autoTcpTest.go",
                    "type": "file",
                    "children": [],
                    "metadata": {
                        "modTime": "2025-05-27T10:42:44.330826892+08:00",
                        "size": 3917
                    }
                },
                {
                    "name": "diffJson.go",
                    "path": "./tools/diffJson.go",
                    "type": "file",
                    "children": [],
                    "metadata": {
                        "modTime": "2025-05-26T15:47:55.273417829+08:00",
                        "size": 4435
                    }
                },
                {
                    "name": "test.go",
                    "path": "./tools/test.go",
                    "type": "file",
                    "children": [],
                    "metadata": {
                        "modTime": "2025-05-27T10:54:39.188757782+08:00",
                        "size": 147
                    }
                }
            ],
            "metadata": {
                "modTime": "2025-05-27T10:53:09.868867046+08:00",
                "size": 160
            }
        }
    ],
    "metadata": {
        "modTime": "2025-05-27T10:18:56.596056391+08:00",
        "size": 544
    }
}`

	jsonB := `{
		"name": "root",
		"path": "/root",
		"type": "dir",
		"children": [
			{
				"name": "file1.txt",
				"path": "/root/file1.txt",
				"type": "dir",
				"children": []
			},
			{
				"name": "subdir",
				"path": "/root/subdir",
				"type": "dir",
				"children": []
			},
			{
				"name": "file3.txt",
				"path": "/root/file3.txt",
				"type": "file",
				"children": []
			}
		]
	}`

	// 比较差异
	diffs, err := findDifferencesFromJSON(jsonA, jsonB)
	if err != nil {
		fmt.Printf("错误: %v\n", err)
		return
	}

	// 输出结果
	fmt.Println("差异列表:")
	for _, diff := range diffs {
		fmt.Printf("路径: %s, 类型: %s, 操作: %s, 名称: %s\n",
			diff.Path, diff.Type, diff.Action, diff.Name)
	}

	// 输出JSON格式
	jsonResult, _ := json.MarshalIndent(diffs, "", "  ")
	fmt.Println("\nJSON格式结果:")
	fmt.Println(string(jsonResult))
}
